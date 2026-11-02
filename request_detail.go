package main

// Per-request detail collection for the request log.
//
// The request log used to record the envelope only — who, when, which path,
// which status. Answering "why was this answer slow / short / sent to the
// wrong engine" meant reading the slog lines beside the row, if they were
// still within retention at all. This file carries the facts the handlers
// already compute (engine chosen, tokens billed, stop reason, time to first
// token) from the moment they are known to the row that stores them.
//
// The mechanism is a context-injected collector: requestLoggingMiddleware
// creates one per request and reads it in its deferred insert, and handlers
// below fill it in place. No locks — the handler goroutine writes and the
// deferred read on that same goroutine reads after ServeHTTP returns.

import (
	"context"
	"net/http"
	"time"
	"unicode/utf8"
)

// requestDetail is the mutable scratchpad one request's handlers write into.
// Every field is optional; zero values render as "—" in the UI.
type requestDetail struct {
	// id correlates the row with the X-Request-ID response header, so an
	// error a client shows on screen traces to exactly one audit row.
	id string
	// start anchors latency and TTFT; set by the middleware at entry.
	start time.Time

	// Routing: what the client asked for vs. what actually served it.
	// They differ when an engine reports an aliased/full tag name back.
	modelRequested string
	modelServed    string
	engineName     string
	engineType     string

	// Outcome, from the engine's own reporting.
	streamed   bool
	stopReason string
	inTokens   int
	outTokens  int

	// Performance. ttft is set once, on the first chunk reaching the client.
	ttft time.Duration
	// duration is stamped by the middleware when ServeHTTP returns — not at
	// flush, which happens later on the writer goroutine and would otherwise
	// count queue wait as request time.
	duration time.Duration

	// upstreamErr is the engine-side failure text for a 502/500, truncated
	// to MaxMsgLen — the reason a row says "Could not reach engine".
	upstreamErr string
	// outcome marks an abnormal end the status code cannot show: a 200 whose
	// stream was interrupted or abandoned mid-flight.
	outcome string

	userAgent string
}

type requestDetailKey struct{}

// newRequestDetail builds the collector for one request and returns it with
// the request already carrying it.
func newRequestDetail(r *http.Request) (*requestDetail, *http.Request) {
	d := &requestDetail{
		id:    newRequestID(),
		start: time.Now(),
		// Bounded by a fixed cap rather than cfg.MaxMsgLen: the header is
		// already limited by the server's per-line read cap, a UA longer
		// than this is noise, and MaxMsgLen is 0 until config loads.
		userAgent: truncateTo(r.Header.Get("User-Agent"), 512),
	}
	return d, r.WithContext(context.WithValue(r.Context(), requestDetailKey{}, d))
}

// truncateTo cuts s to n bytes on a rune boundary.
func truncateTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// detailFromContext returns the collector, or nil when the request did not
// pass through requestLoggingMiddleware (direct handler calls in tests).
// Every setter below is nil-safe for exactly that reason.
func detailFromContext(r *http.Request) *requestDetail {
	d, _ := r.Context().Value(requestDetailKey{}).(*requestDetail)
	return d
}

func (d *requestDetail) setRouted(engineName, engineType, modelRequested string) {
	if d == nil {
		return
	}
	d.engineName, d.engineType, d.modelRequested = engineName, engineType, modelRequested
}

func (d *requestDetail) setStreamed() {
	if d != nil {
		d.streamed = true
	}
}

// applyMeta folds one stream frame's metadata into the collector. Later
// frames overwrite earlier ones, which is the intent: usage and finish
// arrive on the terminal chunk. The served model sticks once set.
func (d *requestDetail) applyMeta(meta chunkMeta) {
	if d == nil {
		return
	}
	if meta.model != "" {
		d.modelServed = meta.model
	}
	if meta.inTokens != 0 || meta.outTokens != 0 {
		d.inTokens, d.outTokens = meta.inTokens, meta.outTokens
	}
	if meta.finish != "" {
		d.stopReason = meta.finish
	}
}

// setFirstToken records time-to-first-token once. Later chunks are ignored:
// the metric of interest is when the client saw anything at all.
func (d *requestDetail) setFirstToken() {
	if d == nil || d.ttft != 0 {
		return
	}
	d.ttft = time.Since(d.start)
}

func (d *requestDetail) setUpstreamErr(err error) {
	if d == nil || err == nil {
		return
	}
	d.upstreamErr = truncateTo(err.Error(), 512)
}

func (d *requestDetail) setOutcome(o string) {
	if d != nil {
		d.outcome = o
	}
}

// newRequestID mints the correlation id, in the style of the message and
// tool-call ids in anthropic.go.
func newRequestID() string { return "req_" + randomHex(8) }

// recordCompletionDetail fills the collector from a non-streaming completion.
// The payload is either a *ChatCompletion (Ollama, after ollamaToOpenAI) or
// the engine's raw JSON map (OpenAI-compat, relayed unmodified), so both
// shapes are read here rather than forcing a marshal round-trip.
func recordCompletionDetail(r *http.Request, payload interface{}) {
	d := detailFromContext(r)
	if d == nil {
		return
	}
	switch p := payload.(type) {
	case *ChatCompletion:
		if p.Model != "" {
			d.modelServed = p.Model
		}
		d.inTokens, d.outTokens = p.Usage.PromptTokens, p.Usage.CompletionTokens
		for _, ch := range p.Choices {
			if ch.FinishReason != nil {
				d.stopReason = *ch.FinishReason
			}
		}
	case map[string]interface{}:
		if m, _ := p["model"].(string); m != "" {
			d.modelServed = m
		}
		if u, ok := p["usage"].(map[string]interface{}); ok {
			d.inTokens, d.outTokens = toInt(u["prompt_tokens"]), toInt(u["completion_tokens"])
		}
		if choices, ok := p["choices"].([]interface{}); ok {
			for _, ci := range choices {
				if ch, ok := ci.(map[string]interface{}); ok {
					if fr, _ := ch["finish_reason"].(string); fr != "" {
						d.stopReason = fr
					}
				}
			}
		}
	}
}

// recordAnthropicDetail fills the collector from a translated Anthropic
// message, whose usage and stop reason are already typed.
func recordAnthropicDetail(r *http.Request, msg *AnthropicMessage) {
	d := detailFromContext(r)
	if d == nil || msg == nil {
		return
	}
	if msg.Model != "" {
		d.modelServed = msg.Model
	}
	d.inTokens, d.outTokens = msg.Usage.InputTokens, msg.Usage.OutputTokens
	if msg.StopReason != nil {
		d.stopReason = *msg.StopReason
	}
}

// chunkMeta is what an adapter learns from one stream frame beyond its text:
// the fields the request log wants, extracted during the parse the adapter
// already performs. Handlers must not re-parse payloads to get these — that
// was removed once already for costing a second full parse of every token.
type chunkMeta struct {
	auditText string
	model     string
	finish    string // OpenAI finish_reason, "" until the terminal chunk
	inTokens  int
	outTokens int
}
