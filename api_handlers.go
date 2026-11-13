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
	"strings"
	"time"
	"unicode/utf8"
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
	//
	// Peek first, warm only when empty: the old warm-then-read blocked every
	// client on the slowest engine for up to the warm budget, and then on any
	// probe that had started earlier with no deadline of its own. Expired
	// cache data still answers here; the refresh runs behind the response.
	snap, stale := cachedProbeSnapshot()
	if len(snap) == 0 {
		// Nothing cached at all (cold start): pay one bounded wait so the
		// first client after a restart gets a catalogue rather than a 502.
		warmCtx, warmCancel := context.WithTimeout(r.Context(), 5*time.Second)
		warmProbeCache(warmCtx)
		warmCancel()
		snap, stale = cachedProbeSnapshot()
	}
	if stale {
		go warmProbeCache(context.Background())
	}

	modelList := make([]Model, 0, 16)
	seen := make(map[string]bool)
	reachable := 0

	for _, e := range engines {
		pd, ok := snap[e.ID]
		if !ok || !pd.status {
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
				OwnedBy:     e.Type,
				Type:        "model",
				DisplayName: name,
				CreatedAt:   time.Unix(int64(created), 0).UTC().Format(time.RFC3339),
				Size:        int64(toInt(m["size"])),
			})
		}
	}

	if reachable == 0 {
		slog.Error("v1/models: no engine reachable", "configured", len(engines))
		jsonResponse(w, 502, openaiError("Could not reach any engine", "api_error"))
		return
	}

	list := ModelList{Object: "list", Data: modelList}
	if len(modelList) > 0 {
		list.FirstID = &modelList[0].ID
		list.LastID = &modelList[len(modelList)-1].ID
	}
	jsonResponse(w, 200, list)
}

// ─────────────────────────────── API: whoami ─────────────────────────────────

// whoamiResponse is the body of GET /v1/whoami. A typed struct rather than a
// map so the wire shape is declared next to the handler that emits it.
type whoamiResponse struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	RateLimitRPM int    `json:"rate_limit_rpm"`
	KeyCreatedAt string `json:"key_created_at"`
}

// handleWhoami reports the identity the caller authenticated as. It is the
// programmatic counterpart of the admin panel's user list: a client that
// wants to confirm which key it is presenting — and that its key is active —
// can ask without reaching the admin UI. The auth middleware has already
// validated the key; a nil user here means the middleware was bypassed (a
// programming error), like the other /v1 handlers. It is a /v1/ route, so
// rateLimitMiddleware covers it like the rest.
func handleWhoami(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		jsonResponse(w, 401, openaiError("Invalid API key", "authentication_error"))
		return
	}
	jsonResponse(w, 200, whoamiResponse{
		Name:         user.Name,
		Status:       user.Status,
		RateLimitRPM: user.RateLimit,
		KeyCreatedAt: user.CreatedAt,
	})
}

// ─────────────────────────────── API: chat completions ────────────────────────

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		jsonResponse(w, 401, openaiError("Invalid API key", "authentication_error"))
		return
	}

	// The per-minute rate limit is enforced by rateLimitMiddleware upstream
	// of this handler, so there is no inline check left here — one token is
	// consumed per request, not one per layer.

	// The daily token quota gates total consumption rather than speed, and
	// checkQuota fails open on any error (a DB hiccup must not block all
	// traffic), so it is safe on the request path. It sits with the other
	// admission checks, before the body is read: an over-quota client costs
	// no parsing.
	if q := checkQuota(user.Name); q.quotaExceeded() {
		jsonResponse(w, 429, openaiError("Daily token quota exceeded", "rate_limit_error"))
		return
	}
	data, ok := decodeAPIBody(w, r, writeOpenAIError)
	if !ok {
		return
	}
	if _, ok := data["messages"]; !ok {
		jsonResponse(w, 400, openaiError("'messages' is required", "invalid_request_error"))
		return
	}

	rawMsgs, _ := data["messages"].([]interface{})
	messages := sanitizeMessages(rawMsgs)
	model, _ := data["model"].(string)

	// Failover routing: routeModelOrdered returns every engine that holds the
	// model, primary first, and each engine's TryAcquire takes a concurrency
	// slot — so a busy or down primary falls through to its failover targets
	// instead of the client seeing a 503.
	engines := routeModelOrdered(model)
	if len(engines) == 0 {
		// nil: nothing is configured, or no engine could be probed at all;
		// empty: engines were probed and none holds the model. Either way no
		// reachable engine serves it, so the client gets the same 404 it
		// always got for a model the fleet does not have.
		jsonResponse(w, 404, openaiError(
			fmt.Sprintf("Model '%s' is not available on any configured engine", model),
			"invalid_request_error"))
		return
	}

	var (
		selected Engine
		found    bool
	)
	for _, eng := range engines {
		// Same backpressure bound as before failover: wait up to 250ms for a
		// slot — long enough to absorb a momentary spike, short enough that
		// clients see a clear backpressure signal. TryAcquire blocks until
		// the context fires, so the timeout is what bounds the wait. With N
		// fallback engines the worst-case wait accumulates to about N×250ms;
		// acceptable for the small engine counts in practice.
		acqCtx, acqCancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
		gotSlot := eng.TryAcquire(acqCtx)
		acqCancel()
		if gotSlot {
			selected, found = eng, true
			break
		}
		// Saturation per engine type, where the busy counter only shows that
		// *some* engine of a type refused. Cardinality is bounded by the
		// engine count.
		metricUpstreamBusy.WithLabelValues(eng.Type()).Inc()
	}
	if !found {
		// Every engine that holds the model is at its concurrency cap.
		w.Header().Set("Retry-After", "2")
		jsonResponse(w, 503, openaiError("Engine busy, please retry", "api_error"))
		return
	}
	detailFromContext(r).setRouted(selected.Name(), selected.Type(), model)
	// In-flight gauge per engine name; the deferred pair returns the slot and
	// the gauge on every exit from this handler.
	metricEngineInflight.WithLabelValues(selected.Name()).Inc()
	defer func() {
		metricEngineInflight.WithLabelValues(selected.Name()).Dec()
		selected.Release()
	}()

	engineReq := buildEngineRequest(selected, data, messages)

	if streaming, _ := data["stream"].(bool); streaming {
		streamCompletion(w, r, selected, engineReq, user.ID, model, messages)
		return
	}

	// Non-streaming. Passing the request context means a client that hangs up
	// cancels the upstream call, which releases the concurrency slot now
	// instead of up to upstreamChatTimeout later.
	result, err := selected.Chat(r.Context(), engineReq)
	if err != nil {
		// Client gone: nothing to write, and this is not an upstream fault —
		// reporting it as a 502 would misattribute the failure in the metrics.
		if r.Context().Err() != nil {
			slog.Info("client disconnected before completion", "model", model, "user", user.ID)
			return
		}
		slog.Error("engine request", "err", err)
		detailFromContext(r).setUpstreamErr(err)
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
	if selected.Type() == EngineOllama {
		payload = ollamaToOpenAI(result)
	}
	recordCompletionDetail(r, payload)

	// Raw values, not marshalled strings: the JSON encoding happens on the
	// writer goroutine, off this request's latency.
	logInteraction(user.ID, messages, payload, model)
	jsonResponse(w, 200, payload)
}

// ─────────────────────────────── API: Anthropic messages ──────────────────────

// handleMessages serves the Anthropic Messages API (POST /v1/messages), which
// is what Claude Code and the Anthropic SDKs call. The request is translated
// to the OpenAI shape and sent down the same path as /v1/chat/completions;
// see anthropic.go.
func handleMessages(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		writeAnthropicError(w, 401, "Invalid API key", "authentication_error")
		return
	}
	// The per-minute rate limit is enforced by rateLimitMiddleware, like for
	// every other /v1/ route; one token per request, no inline check here.

	// The daily token quota is enforced here too, not just in
	// handleChatCompletions: checkQuota sums consumption from both the OpenAI
	// and Anthropic completion paths, so a client that only calls /v1/messages
	// would otherwise burn quota with no gate. Same fail-open semantics.
	if q := checkQuota(user.Name); q.quotaExceeded() {
		writeAnthropicError(w, 429, "Daily token quota exceeded", "rate_limit_error")
		return
	}
	body, ok := decodeAPIBody(w, r, writeAnthropicError)
	if !ok {
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		writeAnthropicError(w, 400, "'model' is required", "invalid_request_error")
		return
	}
	if _, ok := body["messages"].([]interface{}); !ok {
		writeAnthropicError(w, 400, "'messages' is required", "invalid_request_error")
		return
	}

	data, rawMsgs := anthropicToOpenAI(body)
	messages := sanitizeMessages(rawMsgs)

	adapter, release, ok := acquireEngine(w, r, model, writeAnthropicError)
	if !ok {
		return
	}
	defer release()

	engineReq := buildEngineRequest(adapter, data, messages)

	if streaming, _ := data["stream"].(bool); streaming {
		// Asking the engine for usage is openAICompatRequest's job now, and it
		// runs for every non-Ollama adapter — so the Claude Code case (its
		// context window comes from message_delta.usage, and a stream without
		// usage would read as zero tokens) is covered from one place.
		streamMessages(w, r, adapter, engineReq, user.ID, model, messages)
		return
	}

	result, err := adapter.Chat(r.Context(), engineReq)
	if err != nil {
		if r.Context().Err() != nil {
			slog.Info("client disconnected before completion", "model", model, "user", user.ID)
			return
		}
		slog.Error("engine request", "err", err)
		detailFromContext(r).setUpstreamErr(err)
		writeAnthropicError(w, 502, "Could not reach engine", "api_error")
		return
	}
	var payload interface{} = result
	if adapter.Type() == EngineOllama {
		payload = ollamaToOpenAI(result)
	}
	msg, err := openAIToAnthropic(payload, model)
	if err != nil {
		slog.Error("translate engine response", "err", err, "model", model)
		writeAnthropicError(w, 502, "Unexpected engine response", "api_error")
		return
	}
	recordAnthropicDetail(r, msg)
	logInteraction(user.ID, messages, msg, model)
	jsonResponse(w, 200, msg)
}

// streamMessages relays a streaming completion as Anthropic SSE events. It
// mirrors streamCompletion — same deadline, disconnect and audit handling —
// with anthropicStream turning each OpenAI chunk into typed events.
func streamMessages(w http.ResponseWriter, r *http.Request, adapter Engine,
	engineReq map[string]interface{}, userID int, model string,
	messages []map[string]interface{}) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, 500, "Streaming not supported", "api_error")
		return
	}

	deadline := newStreamDeadline(r.Context(), streamIdle, streamMax)
	defer deadline.stop()

	stream, err := adapter.StreamChat(deadline.ctx, engineReq)
	if err != nil {
		if r.Context().Err() != nil {
			slog.Info("client disconnected before stream started", "model", model, "user", userID)
			return
		}
		slog.Error("engine stream", "err", err, "model", model)
		detailFromContext(r).setUpstreamErr(err)
		writeAnthropicError(w, 502, "Could not reach engine", "api_error")
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	audit := streamAudit{limit: cfg.MaxMsgLen}
	var outcome string
	defer func() {
		resp := audit.String()
		if outcome != "" {
			resp += "\n[" + outcome + "]"
		}
		logInteraction(userID, messages, resp, model)
	}()

	send := func(events ...sseEvent) bool {
		for _, ev := range events {
			b, err := json.Marshal(ev.data)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, b); err != nil {
				return false
			}
		}
		flusher.Flush()
		return true
	}

	detail := detailFromContext(r)
	detail.setStreamed()

	tr := newAnthropicStream(model)
	if !send(tr.start()) {
		outcome = "client disconnected"
		return
	}
	for {
		payload, meta, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Anthropic's in-stream error event: the client surfaces it
			// instead of treating the truncated message as complete. The
			// deadline cause replaces the read error when the gateway capped
			// the stream, so the audit can tell that from a transport failure.
			why := err
			if cause := deadline.cause(); cause != nil {
				why = cause
			}
			slog.Error("engine stream read", "err", why, "model", model)
			send(sseEvent{"error", anthropicError(500, "Stream interrupted", "api_error")})
			outcome = "stream interrupted"
			detail.setOutcome(outcome)
			detail.setUpstreamErr(why)
			return
		}
		// Touched on the read, not after the write — the bound tracks whether
		// the engine is alive, not how fast this client drains.
		deadline.touch()
		if !send(tr.chunk(payload)...) {
			slog.Info("client disconnected mid-stream", "model", model, "user", userID)
			outcome = "client disconnected"
			detail.setOutcome(outcome)
			return
		}
		detail.setFirstToken()
		detail.applyMeta(meta)
		audit.write(meta.auditText)
	}
	send(tr.end()...)
}

// handleCountTokens serves POST /v1/messages/count_tokens with an estimate;
// see estimateTokens for why an estimate is enough.
func handleCountTokens(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		writeAnthropicError(w, 401, "Invalid API key", "authentication_error")
		return
	}
	// Rate limited like the completion endpoints by rateLimitMiddleware: no
	// engine is involved, but each call still decodes and re-encodes a body
	// of up to 8× MaxMsgLen.
	body, ok := decodeAPIBody(w, r, writeAnthropicError)
	if !ok {
		return
	}
	jsonResponse(w, 200, map[string]int{
		"input_tokens": estimateTokens(body["system"], body["messages"], body["tools"]),
	})
}

// ─────────────────────────────── Shared admission ─────────────────────────────

// apiErrorWriter writes an error in one API's envelope. The admission steps
// below are shared by /v1/chat/completions and /v1/messages; only the shape
// of the error body differs between them.
type apiErrorWriter func(w http.ResponseWriter, status int, message, errType string)

func writeOpenAIError(w http.ResponseWriter, status int, message, errType string) {
	jsonResponse(w, status, openaiError(message, errType))
}

func writeAnthropicError(w http.ResponseWriter, status int, message, errType string) {
	jsonResponse(w, status, anthropicError(status, message, errType))
}

// decodeAPIBody reads a bounded JSON object body.
//
// The bound is ~8× MaxMsgLen so a multi-turn conversation of bounded-length
// messages still fits; truncateInput then trims each field.
func decodeAPIBody(w http.ResponseWriter, r *http.Request, writeErr apiErrorWriter) (map[string]interface{}, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(cfg.MaxMsgLen)*8)
	var data map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil || data == nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, 413, "Request body too large", "invalid_request_error")
			return nil, false
		}
		writeErr(w, 400, "Invalid JSON body", "invalid_request_error")
		return nil, false
	}
	return data, true
}

// acquireEngine routes model to an engine and takes one of its concurrency
// slots. On success the caller must defer release; on failure the error has
// already been written.
func acquireEngine(w http.ResponseWriter, r *http.Request, model string,
	writeErr apiErrorWriter) (adapter Engine, release func(), ok bool) {

	// Route on the requested model rather than always taking the first engine,
	// so a multi-engine deployment reaches whichever host actually holds it.
	adapter, rerr := selectEngine(model)
	switch rerr {
	case routeNoEngines:
		writeErr(w, 502, "No engines configured", "api_error")
		return nil, nil, false
	case routeModelNotFound:
		writeErr(w, 404,
			fmt.Sprintf("Model '%s' is not available on any configured engine", model),
			"invalid_request_error")
		return nil, nil, false
	}
	detailFromContext(r).setRouted(adapter.Name(), adapter.Type(), model)
	// Cap upstream concurrency. Wait up to 250ms for a slot before giving
	// up — long enough to absorb a momentary spike, short enough that
	// clients see a clear backpressure signal.
	acqCtx, acqCancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
	gotSlot := adapter.TryAcquire(acqCtx)
	acqCancel()
	if !gotSlot {
		metricUpstreamBusy.WithLabelValues(adapter.Type()).Inc()
		w.Header().Set("Retry-After", "2")
		writeErr(w, 503, "Engine busy, please retry", "api_error")
		return nil, nil, false
	}
	// Saturation per engine, where the busy counter only shows that *some*
	// engine of a type refused. Cardinality is bounded by the engine count.
	metricEngineInflight.WithLabelValues(adapter.Name()).Inc()
	return adapter, func() {
		metricEngineInflight.WithLabelValues(adapter.Name()).Dec()
		adapter.Release()
	}, true
}

// buildEngineRequest translates the OpenAI-shaped request for the engine —
// only Ollama adapters need it.
func buildEngineRequest(adapter Engine, data map[string]interface{}, messages []map[string]interface{}) map[string]interface{} {
	if adapter.Type() == EngineOllama {
		return openAIToOllama(data, messages)
	}
	return openAICompatRequest(data, messages)
}

// openAICompatRequest builds the body for an OpenAI-compatible engine: the
// sanitised messages plus the client's parameters that are on the allow-list.
//
// model, messages and stream are set here and deliberately absent from the
// allow-list. messages used to be on it, which overwrote the sanitised slice
// with the client's raw one, so truncation never reached this engine type.
// stream is always a bool: the handler reads a missing value as "not
// streaming", and forwarding the absence as null would leave the upstream to
// decide — or, on a strict one, reject the request.
//
// A stream carries usage only if the engine was asked for it, so this is where
// the gateway asks. Without include_usage every streamed completion to this
// engine type logs zero tokens, which the request log and the usage panel both
// read as a free request rather than an unmeasured one. Ollama needs nothing:
// it reports counts on its final line. This is the single owner for both
// front-ends, since buildEngineRequest routes here for every non-Ollama
// adapter. A caller's own stream_options sub-fields are carried through, with
// include_usage forced on — usage on a response the gateway is auditing is
// never unwanted, and an accurate audit is not negotiable.
func openAICompatRequest(data map[string]interface{}, messages []map[string]interface{}) map[string]interface{} {
	streaming, _ := data["stream"].(bool)
	req := map[string]interface{}{
		"model":    data["model"],
		"messages": messages,
		"stream":   streaming,
	}
	for key := range data {
		switch key {
		case "temperature", "top_p", "seed", "max_tokens",
			"max_completion_tokens", "frequency_penalty", "presence_penalty", "stop",
			"provider_options", "response_format", "service_tier", "logit_bias", "tools",
			"tool_choice", "logprobs", "top_logprobs", "parallel_tool_calls",
			"user", "n":
			req[key] = data[key]
		}
	}
	if streaming {
		// Copied rather than written through: the decoded request belongs to
		// the caller, and sanitizeContent established that these builders do not
		// mutate it behind the caller's back.
		client, _ := data["stream_options"].(map[string]interface{})
		so := make(map[string]interface{}, len(client)+1)
		for k, v := range client {
			so[k] = v
		}
		so["include_usage"] = true
		req["stream_options"] = so
	}
	return req
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
	// It bounds silence rather than total duration (see streamDeadline): the
	// old whole-stream cap cut healthy generations. Deriving from r.Context()
	// still means a client hanging up cancels the upstream read immediately,
	// which releases the engine's concurrency slot at once.
	deadline := newStreamDeadline(r.Context(), streamIdle, streamMax)
	defer deadline.stop()

	stream, err := adapter.StreamChat(deadline.ctx, engineReq)
	if err != nil {
		// Nothing has been written yet, so a real status code is still
		// possible — better than a 200 carrying an error-shaped SSE frame,
		// which is what the old per-adapter code had to do. Only the request
		// context says whether the client left; a deadline that fired here is
		// reported upstream instead.
		if r.Context().Err() != nil {
			slog.Info("client disconnected before stream started", "model", model, "user", userID)
			return
		}
		slog.Error("engine stream", "err", err, "model", model)
		detailFromContext(r).setUpstreamErr(err)
		jsonResponse(w, 502, openaiError("Could not reach engine", "api_error"))
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Audited on every exit from here on, including a client that hangs up:
	// the interaction log is a record of what each client was told, and a
	// partial answer is still something it was told. outcome marks a
	// completion that did not end normally.
	audit := streamAudit{limit: cfg.MaxMsgLen}
	var outcome string
	detail := detailFromContext(r)
	detail.setStreamed()
	defer func() {
		resp := audit.String()
		if outcome != "" {
			resp += "\n[" + outcome + "]"
			detail.setOutcome(outcome)
		}
		logInteraction(userID, messages, resp, model)
	}()

	for {
		payload, meta, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Headers are already out, so the status cannot change. Emitting
			// an error frame before [DONE] is the only way to tell a client
			// that the completion it received is truncated.
			//
			// The deadline cause replaces the read error when the gateway
			// itself ended the stream: Recv can only report a cancelled read,
			// which would hide that this was a timeout rather than a network
			// failure — the gap that made the old cap uninterpretable in the
			// request log.
			why := err
			if cause := deadline.cause(); cause != nil {
				why = cause
			}
			slog.Error("engine stream read", "err", why, "model", model)
			if b, mErr := json.Marshal(openaiError("Stream interrupted", "api_error")); mErr == nil {
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			outcome = "stream interrupted"
			detail.setUpstreamErr(why)
			break
		}
		// Touched on the read, not after the write: the bound measures whether
		// the engine is alive, and a client that drains slowly must not get its
		// own completion killed for reading it carefully.
		deadline.touch()
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			// Client went away mid-stream. Returning unwinds the deferred
			// Close and Release rather than writing into a dead connection
			// for the rest of the completion.
			slog.Info("client disconnected mid-stream", "model", model, "user", userID)
			outcome = "client disconnected"
			return
		}
		detail.setFirstToken()
		detail.applyMeta(meta)
		// Recorded after the write succeeds, so the audit holds what reached
		// the client rather than what the engine produced. The text was
		// extracted by the adapter while it parsed the frame — no second
		// unmarshal here.
		audit.write(meta.auditText)
		flusher.Flush()
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// sanitizeMessages bounds each message's text and keeps the fields a chat
// message can legitimately carry.
//
// It used to rebuild every message as {role, content-as-string}. That
// silently broke tool use for every engine type: an assistant turn lost its
// tool_calls and a tool result lost its tool_call_id, so the second request of
// any tool loop was rejected upstream. Array content (text and image parts)
// was flattened to "" for the same reason.
func sanitizeMessages(messages []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		clean := map[string]interface{}{
			"role":    role,
			"content": sanitizeContent(msg["content"]),
		}
		for _, key := range []string{"name", "tool_call_id", "tool_calls"} {
			if v, ok := msg[key]; ok && v != nil {
				clean[key] = v
			}
		}
		// An assistant turn that only calls tools has null content in the
		// OpenAI schema; keep it null there. Anywhere else, absent content
		// becomes "" — what this function always sent — since strict
		// upstreams reject a user message whose content is null.
		if clean["content"] == nil && clean["tool_calls"] == nil {
			clean["content"] = ""
		}
		out = append(out, clean)
	}
	return out
}

// sanitizeContent truncates the text in a message's content, which is either a
// string or an array of typed parts. Parts are copied, never edited in place,
// so the caller's decoded request is not modified behind its back. Non-text
// parts pass through: their size is already bounded by the request body limit.
func sanitizeContent(content interface{}) interface{} {
	switch c := content.(type) {
	case string:
		return truncateInput(c)
	case []interface{}:
		parts := make([]interface{}, 0, len(c))
		for _, p := range c {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if text, isText := part["text"].(string); isText && part["type"] == "text" {
				cp := make(map[string]interface{}, len(part))
				for k, v := range part {
					cp[k] = v
				}
				cp["text"] = truncateInput(text)
				part = cp
			}
			parts = append(parts, part)
		}
		return parts
	}
	return nil
}

// streamAudit accumulates a streamed completion for the interaction log from
// the audit text each chunk carries (see ChatStream.Recv): content deltas
// joined, and each tool call as "[tool_call name] arguments".
//
// It is capped at limit bytes because a stream has no length bound of its
// own, and enqueueInteractionLog would truncate to MaxMsgLen anyway —
// buffering past that only costs memory.
type streamAudit struct {
	limit int
	buf   strings.Builder
}

// write appends s up to the limit, cutting on a rune boundary.
func (a *streamAudit) write(s string) {
	room := a.limit - a.buf.Len()
	if room <= 0 || s == "" {
		return
	}
	if len(s) > room {
		for room > 0 && !utf8.RuneStart(s[room]) {
			room--
		}
		s = s[:room]
	}
	a.buf.WriteString(s)
}

func (a *streamAudit) String() string { return strings.TrimPrefix(a.buf.String(), "\n") }
