package main

// HTTP handlers for the OpenAI-compatible /v1 surface, plus the two unauthenticated
// endpoints that exist for the same audience (CORS preflight and health).
//
// Split out of handlers.go, which had grown past 1,300 lines mixing this with the
// admin web UI. The two surfaces have almost nothing in common: this one is
// bearer-authenticated, returns JSON and SSE, and is consumed by OpenAI SDK
// clients; the other is cookie-authenticated, returns HTML, and is consumed by a
// browser. Keeping them in separate files means a change to the admin panel cannot
// accidentally alter the API contract, and the /v1 contract can be read end to end.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// ─────────────────────────────── CORS preflight ────────────────────────────────

// handleCorsPreflight handles CORS preflight requests
func handleCorsPreflight(w http.ResponseWriter, r *http.Request) {
	for k, v := range corsHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────────── Health ───────────────────────────────────────

// handleHealth returns a simple health check response
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// ─────────────────────────────── API: models ──────────────────────────────────

func handleListModels(w http.ResponseWriter, r *http.Request) {
	// Auth already resolved by apiAuthMiddleware; if user is nil here, the
	// middleware was bypassed (programming error).
	if userFromContext(r) == nil {
		jsonResponse(w, 401, openaiError("Invalid API key", "authentication_error"))
		return
	}

	engines := cachedEngines()
	if len(engines) == 0 {
		jsonResponse(w, 502, openaiError("No engines configured", "api_error"))
		return
	}

	// Aggregate across every engine, not just the first. Reading from the
	// probe cache means this costs no upstream calls in the common case, and
	// an engine that is down contributes nothing instead of failing the whole
	// listing — a partial catalogue is more useful than a 502.
	warmCtx, warmCancel := context.WithTimeout(r.Context(), 5*time.Second)
	warmProbeCache(warmCtx)
	warmCancel()

	modelList := make([]Model, 0, 16)
	seen := make(map[string]bool)
	reachable := 0

	for _, e := range engines {
		adapter := cachedAdapter(e.ID)
		if adapter == nil {
			continue
		}
		pd := probeEngineCached(e.ID, adapter)
		if !pd.status {
			continue
		}
		reachable++
		for _, m := range pd.raw {
			name := modelName(m)
			if name == "" || seen[name] {
				// Same model on two engines is one entry in the catalogue;
				// selectEngine decides which host serves it.
				continue
			}
			seen[name] = true

			// Ollama reports "modified_at"; OpenAI-compat engines already send
			// a numeric "created". Fall back to now rather than 0, which some
			// clients render as 1970.
			created, _ := m["created"].(float64)
			if created == 0 {
				ts, _ := m["modified_at"].(string)
				created = float64(parseOllamaTS(ts))
			}
			modelList = append(modelList, Model{
				ID:      name,
				Object:  "model",
				Created: int64(created),
				// The engine that actually holds this model, not a blanket
				// value taken from engines[0].
				OwnedBy: e.Type,
				Size:    int64(toInt(m["size"])),
			})
		}
	}

	if reachable == 0 {
		slog.Error("v1/models: no engine reachable", "configured", len(engines))
		jsonResponse(w, 502, openaiError("Could not reach any engine", "api_error"))
		return
	}

	jsonResponse(w, 200, ModelList{Object: "list", Data: modelList})
}

// ─────────────────────────────── API: chat completions ────────────────────────

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		jsonResponse(w, 401, openaiError("Invalid API key", "authentication_error"))
		return
	}

	if !isAllowed(user.ID, user.RateLimit) {
		metricRateLimited.Inc()
		jsonResponse(w, 429, openaiError("Rate limit exceeded", "rate_limit_error"))
		return
	}

	// Bound the request body. Allow ~8× MaxMsgLen so a multi-turn conversation
	// of bounded-length messages still fits; truncateInput then trims each field.
	r.Body = http.MaxBytesReader(w, r.Body, int64(cfg.MaxMsgLen)*8)

	var data map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil || data == nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonResponse(w, 413, openaiError("Request body too large", "invalid_request_error"))
			return
		}
		jsonResponse(w, 400, openaiError("Invalid JSON body", "invalid_request_error"))
		return
	}
	if _, ok := data["messages"]; !ok {
		jsonResponse(w, 400, openaiError("'messages' is required", "invalid_request_error"))
		return
	}

	rawMsgs, _ := data["messages"].([]interface{})
	messages := sanitizeMessages(rawMsgs)
	model, _ := data["model"].(string)

	// Route on the requested model rather than always taking the first engine,
	// so a multi-engine deployment reaches whichever host actually holds it.
	adapter, rerr := selectEngine(model)
	switch rerr {
	case routeNoEngines:
		jsonResponse(w, 502, openaiError("No engines configured", "api_error"))
		return
	case routeModelNotFound:
		jsonResponse(w, 404, openaiError(
			fmt.Sprintf("Model '%s' is not available on any configured engine", model),
			"invalid_request_error"))
		return
	}

	// Cap upstream concurrency. Wait up to 250ms for a slot before giving
	// up — long enough to absorb a momentary spike, short enough that
	// clients see a clear backpressure signal.
	acqCtx, acqCancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
	gotSlot := adapter.TryAcquire(acqCtx)
	acqCancel()
	if !gotSlot {
		metricUpstreamBusy.WithLabelValues(adapter.Type()).Inc()
		w.Header().Set("Retry-After", "2")
		jsonResponse(w, 503, openaiError("Engine busy, please retry", "api_error"))
		return
	}
	defer adapter.Release()

	// Build the request for the engine — translate only for Ollama adapters.
	var engineReq map[string]interface{}
	if adapter.Type() == EngineOllama {
		engineReq = openAIToOllama(data, messages)
	} else {
		engineReq = map[string]interface{}{
			"model":    data["model"],
			"messages": messages,
			"stream":   data["stream"],
		}
		// pass through recognized OpenAI params
		for key := range data {
			switch key {
			case "model", "messages", "stream", "temperature", "top_p", "seed", "max_tokens",
				"max_completion_tokens", "frequency_penalty", "presence_penalty", "stop",
				"provider_options", "response_format", "service_tier", "logit_bias", "tools",
				"tool_choice", "logprobs", "top_logprobs", "parallel_tool_calls",
				"user", "n":
				engineReq[key] = data[key]
			}
		}
	}

	if streaming, _ := data["stream"].(bool); streaming {
		streamCompletion(w, r, adapter, engineReq, user.ID, model, messages)
		return
	}

	// Non-streaming. Passing the request context means a client that hangs up
	// cancels the upstream call, which releases the concurrency slot now
	// instead of up to upstreamChatTimeout later.
	result, err := adapter.Chat(r.Context(), engineReq)
	if err != nil {
		// Client gone: nothing to write, and this is not an upstream fault —
		// reporting it as a 502 would misattribute the failure in the metrics.
		if r.Context().Err() != nil {
			slog.Info("client disconnected before completion", "model", model, "user", user.ID)
			return
		}
		slog.Error("engine request", "err", err)
		jsonResponse(w, 502, openaiError("Could not reach engine", "api_error"))
		return
	}

	// Ollama's /api/chat answers with its own envelope
	// ({model, message:{…}, done, eval_count}); clients calling this gateway
	// expect the OpenAI chat.completion shape. The streaming path already
	// translates, so the non-streaming one has to as well.
	//
	// An OpenAI-compatible engine's body is relayed as-is — typing it would
	// mean dropping any field this gateway does not model, and a passthrough
	// that silently truncates the upstream response is worse than an untyped
	// one.
	var payload interface{} = result
	if adapter.Type() == EngineOllama {
		payload = ollamaToOpenAI(result)
	}

	logInteraction(user.ID, marshalAudit(messages), marshalAudit(payload), model)
	jsonResponse(w, 200, payload)
}

// streamCompletion relays a streaming completion to the client as SSE.
//
// The wire format lives here rather than in each adapter: both engine types
// now hand back a ChatStream of OpenAI-shaped payloads, so framing, flushing,
// the terminating [DONE] and the audit record are written once instead of
// being duplicated (and drifting) per adapter.
func streamCompletion(w http.ResponseWriter, r *http.Request, adapter Engine,
	engineReq map[string]interface{}, userID int, model string,
	messages []map[string]interface{}) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, 500, openaiError("Streaming not supported", "api_error"))
		return
	}

	// The adapter's stream outlives StreamChat, so the deadline is owned here.
	// Deriving from r.Context() means a client hanging up cancels the upstream
	// read, which releases the engine's concurrency slot immediately.
	ctx, cancel := context.WithTimeout(r.Context(), upstreamChatTimeout)
	defer cancel()

	stream, err := adapter.StreamChat(ctx, engineReq)
	if err != nil {
		// Nothing has been written yet, so a real status code is still
		// possible — better than a 200 carrying an error-shaped SSE frame,
		// which is what the old per-adapter code had to do.
		if ctx.Err() != nil && r.Context().Err() != nil {
			slog.Info("client disconnected before stream started", "model", model, "user", userID)
			return
		}
		slog.Error("engine stream", "err", err, "model", model)
		jsonResponse(w, 502, openaiError("Could not reach engine", "api_error"))
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		payload, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Headers are already out, so the status cannot change. Emitting
			// an error frame before [DONE] is the only way to tell a client
			// that the completion it received is truncated.
			slog.Error("engine stream read", "err", err, "model", model)
			if b, mErr := json.Marshal(openaiError("Stream interrupted", "api_error")); mErr == nil {
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			break
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			// Client went away mid-stream. Returning unwinds the deferred
			// Close and Release rather than writing into a dead connection
			// for the rest of the completion.
			slog.Info("client disconnected mid-stream", "model", model, "user", userID)
			return
		}
		flusher.Flush()
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()

	logInteraction(userID, marshalAudit(messages), "streamed", model)
}

func sanitizeMessages(messages []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)
		out = append(out, map[string]interface{}{
			"role":    role,
			"content": truncateInput(content),
		})
	}
	return out
}
