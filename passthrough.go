package main

// Pass-through OpenAI endpoints: /v1/responses, /v1/completions, /v1/audio/*,
// /v1/images/generations and /v1/moderations.
//
// These are relayed to the engine's own implementation rather than
// translated. oMLX, vLLM and LM Studio implement most of them, each faithfully in its own way; a
// translation onto chat completions would lose what makes them different
// (the Responses API's items and event stream, audio and image bodies). An
// engine without the endpoint answers 404, and the client gets that 404 with
// the engine's reason.
//
// Admission is the chat path's — API key, rate limit, daily quota, routing by
// the body's model with failover, a concurrency slot — and the request log
// records the usage the engine reports, so quotas count these calls too.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// passthroughPaths are the routes handled by handlePassthrough.
var passthroughPaths = []string{
	"/v1/responses",
	"/v1/completions",
	"/v1/audio/speech",
	"/v1/audio/transcriptions",
	"/v1/audio/translations",
	"/v1/images/generations",
	"/v1/moderations",
}

// maxRelayedJSON bounds a non-streaming JSON response the gateway reads whole
// to find its usage block. Binary bodies (audio, images as bytes) are copied
// without being held.
const maxRelayedJSON = 64 << 20

func handlePassthrough(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		writeOpenAIError(w, 401, "Invalid API key", "authentication_error")
		return
	}
	if q := checkQuota(user.Name); q.quotaExceeded() {
		writeOpenAIError(w, 429, "Daily token quota exceeded", "rate_limit_error")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes()))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeOpenAIError(w, 413, "Request body too large", "invalid_request_error")
			return
		}
		writeOpenAIError(w, 400, "Could not read request body", "invalid_request_error")
		return
	}

	contentType := r.Header.Get("Content-Type")
	model, streaming, body, err := inspectPassthroughBody(r.URL.Path, contentType, raw)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, errPassthroughJSON) {
			msg = "Invalid JSON body" // as on the other JSON routes
		}
		writeOpenAIError(w, 400, msg, "invalid_request_error")
		return
	}

	// An empty model routes to the default engine, as for chat; moderations
	// make model optional.
	adapter, release, ok := acquireEngine(w, r, model, writeOpenAIError)
	if !ok {
		return
	}
	defer release()

	target, apiKey := passthroughTarget(adapter, r.URL.Path)
	if target == "" {
		writeOpenAIError(w, 400, "The engine serving this model does not support this endpoint", "invalid_request_error")
		return
	}

	// A stream is bounded by silence, as for chat (every byte counts as
	// activity); anything else by the total upstream budget.
	var ctx context.Context
	var deadline *streamDeadline
	if streaming {
		deadline = newStreamDeadline(r.Context(), streamIdle, streamMax)
		defer deadline.stop()
		ctx = deadline.ctx
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), upstreamChatTimeout())
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		writeOpenAIError(w, 500, "Could not build engine request", "api_error")
		return
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := completionClient
	if streaming {
		client = sharedClient
	}
	start := time.Now()
	resp, err := client.Do(req)
	recordUpstream(adapter.Type(), "passthrough", start, err)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		slog.Error("engine passthrough", "err", err, "path", r.URL.Path, "model", model)
		detailFromContext(r).setUpstreamErr(err)
		writeOpenAIEngineFailure(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ue := newUpstreamStatusError(resp)
		detailFromContext(r).setUpstreamErr(ue)
		writeOpenAIEngineFailure(w, ue)
		return
	}

	for _, h := range []string{"Content-Type", "Content-Disposition", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	respType := resp.Header.Get("Content-Type")
	detail := detailFromContext(r)
	switch {
	case strings.HasPrefix(respType, "text/event-stream"):
		var src io.Reader = resp.Body
		if deadline != nil {
			src = touchOnRead(deadline.ctx, resp.Body)
		}
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		detail.setStreamed()
		relaySSE(w, src, detail)
	case strings.Contains(respType, "json"):
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayedJSON))
		if err != nil {
			writeOpenAIEngineFailure(w, err)
			return
		}
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) == nil {
			if in, out, ok := passthroughUsage(payload); ok {
				detail.setTokens(in, out)
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	default:
		// Audio and other binary bodies: copied as they arrive.
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
	}
}

// errPassthroughJSON is a JSON route's body that does not decode.
var errPassthroughJSON = errors.New("invalid JSON body")

// inspectPassthroughBody finds the model a request routes on and whether it
// streams. JSON bodies are decoded for that; a streaming /v1/completions
// request is given stream_options.include_usage, as chat completions are, so
// the request log can count it. Multipart bodies (audio uploads) are scanned
// for their "model" field and forwarded byte for byte.
func inspectPassthroughBody(path, contentType string, raw []byte) (model string, streaming bool, body []byte, err error) {
	mt, params, _ := mime.ParseMediaType(contentType)
	if mt == "multipart/form-data" {
		mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			part, perr := mr.NextPart()
			if perr != nil {
				break
			}
			if part.FormName() == "model" {
				b, _ := io.ReadAll(io.LimitReader(part, 512))
				model = strings.TrimSpace(string(b))
				_ = part.Close()
				break
			}
			_ = part.Close()
		}
		return model, false, raw, nil
	}

	var data map[string]interface{}
	if jerr := json.Unmarshal(raw, &data); jerr != nil || data == nil {
		return "", false, nil, errPassthroughJSON
	}
	model, _ = data["model"].(string)
	streaming, _ = data["stream"].(bool)
	if model == "" && path != "/v1/moderations" {
		return "", false, nil, errors.New("'model' is required")
	}
	if streaming && path == "/v1/completions" {
		so, _ := data["stream_options"].(map[string]interface{})
		cp := make(map[string]interface{}, len(so)+1)
		for k, v := range so {
			cp[k] = v
		}
		cp["include_usage"] = true
		data["stream_options"] = cp
		raw, _ = json.Marshal(data)
	}
	return model, streaming, raw, nil
}

// passthroughTarget is the engine URL for path: the engine's own route.
func passthroughTarget(adapter Engine, path string) (url, apiKey string) {
	if a, ok := adapter.(*OpenAICompatAdapter); ok {
		return a.upstream(path), a.cfg.APIKey
	}
	return "", ""
}

// relaySSE copies an SSE stream line for line, flushing at each event
// boundary, and records the usage found in the stream: a completions chunk's
// usage, or the Responses API's response.completed event.
func relaySSE(w http.ResponseWriter, src io.Reader, detail *requestDetail) {
	flusher, _ := w.(http.Flusher)
	scanner := newStreamScanner(src)
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return // client gone
		}
		if first && line != "" {
			detail.setFirstToken()
			first = false
		}
		if line == "" && flusher != nil {
			flusher.Flush()
		}
		if payload, ok := strings.CutPrefix(line, "data:"); ok && strings.Contains(payload, `"usage"`) {
			var m map[string]interface{}
			if json.Unmarshal([]byte(strings.TrimSpace(payload)), &m) == nil {
				if in, out, ok := passthroughUsage(m); ok {
					detail.setTokens(in, out)
				}
			}
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// passthroughUsage reads token counts from a response or event: OpenAI's
// usage block (prompt/completion tokens, or the Responses API's input/output
// tokens), at the top level or inside a Responses API event's "response".
func passthroughUsage(m map[string]interface{}) (in, out int, ok bool) {
	u, _ := m["usage"].(map[string]interface{})
	if u == nil {
		if resp, _ := m["response"].(map[string]interface{}); resp != nil {
			u, _ = resp["usage"].(map[string]interface{})
		}
	}
	if u == nil {
		return 0, 0, false
	}
	in = max(toInt(u["prompt_tokens"]), toInt(u["input_tokens"]))
	out = max(toInt(u["completion_tokens"]), toInt(u["output_tokens"]))
	return in, out, in > 0 || out > 0
}
