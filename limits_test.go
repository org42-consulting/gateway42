package main

// What the gateway must not change about a request's size: the output budget
// (max_tokens) and the conversation it carries (the context window). Claude
// Desktop / Cowork / Claude Code send the whole session on every turn, with
// max_tokens sized for the model, and the gateway is only a translator.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMaxTokensPassesThroughUnchanged(t *testing.T) {
	body := decodeJSON(t, `{"model":"m","max_tokens":64000,"messages":[{"role":"user","content":"hi"}]}`).(map[string]interface{})
	data, raw := anthropicToOpenAI(body)
	msgs := sanitizeMessages(raw)

	compat, _ := json.Marshal(openAICompatRequest(data, msgs))
	if !strings.Contains(string(compat), `"max_tokens":64000`) {
		t.Errorf("engine request lost max_tokens: %s", compat)
	}
}

// No ceiling or default is invented: without max_tokens the engine decides.
func TestNoMaxTokensInventedWhenAbsent(t *testing.T) {
	body := decodeJSON(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`).(map[string]interface{})
	data, raw := anthropicToOpenAI(body)
	msgs := sanitizeMessages(raw)
	b := mustJSON(t, openAICompatRequest(data, msgs))
	if strings.Contains(string(b), "max_tokens") || strings.Contains(string(b), "max_completion_tokens") {
		t.Errorf("gateway invented a length limit: %s", b)
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A long session's body is far beyond the old 8 × MAX_MESSAGE_LENGTH (2 MiB);
// it must decode, while the configured bound still answers 413.
func TestRequestBodyBoundIsIndependentOfMessageLength(t *testing.T) {
	withMaxMsgLen(t, 262144)
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 6<<20) + `"}]}`

	decode := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(big))
		if _, ok := decodeAPIBody(rec, req, writeAnthropicError); ok {
			return http.StatusOK
		}
		return rec.Code
	}
	if code := decode(); code != http.StatusOK {
		t.Fatalf("6 MiB body: status %d, want it accepted under the 32 MiB default", code)
	}

	prev := cfg.MaxRequestBytes
	cfg.MaxRequestBytes = 1 << 20
	t.Cleanup(func() { cfg.MaxRequestBytes = prev })
	if code := decode(); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over GW42_MAX_REQUEST_BYTES: status %d, want 413", code)
	}
}

// oMLX keeps a stream alive during a long prefill with SSE comments, which
// the frame parser skips. Those bytes must still count as engine activity, or
// a prompt that takes longer than streamIdle to prefill is cut off.
func TestKeepAliveCommentsHoldTheIdleDeadline(t *testing.T) {
	d := newStreamDeadline(t.Context(), 80*time.Millisecond, time.Minute)
	defer d.stop()

	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 8; i++ { // 8 × 30ms = 240ms of "prefill", 3× the idle gap
			time.Sleep(30 * time.Millisecond)
			pw.Write([]byte(": keep-alive\n\n"))
		}
		pw.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"))
		pw.Close()
	}()

	body := touchOnRead(d.ctx, pr)
	s := &passthroughStream{body: io.NopCloser(body), scanner: newStreamScanner(body)}
	payload, _, err := s.Recv()
	if err != nil || !bytes.Contains(payload, []byte(`"hi"`)) {
		t.Fatalf("Recv = %q, %v", payload, err)
	}
	if cause := d.cause(); cause != nil {
		t.Fatalf("deadline fired during keep-alives: %v", cause)
	}
}

// Silence still ends a stream: the wrapper counts bytes, not time.
func TestSilentEngineStillHitsIdleDeadline(t *testing.T) {
	d := newStreamDeadline(t.Context(), 40*time.Millisecond, time.Minute)
	defer d.stop()
	pr, pw := io.Pipe()
	defer pw.Close()
	_ = touchOnRead(d.ctx, pr)
	select {
	case <-d.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("idle deadline never fired")
	}
	if d.cause() != errStreamIdle {
		t.Fatalf("cause = %v, want errStreamIdle", d.cause())
	}
}
