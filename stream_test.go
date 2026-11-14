package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// These tests exist because ChatStream is a reader rather than something that
// writes into an http.ResponseWriter. The translators can be driven straight
// from a string, with no httptest recorder and no parsing SSE framing back off
// the wire to find out what the adapter decided.

// nopCloser lets a strings.Reader stand in for an http response body.
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

// drain collects every payload a stream yields, plus the terminating error.
// The audit text each Recv carries alongside is checked separately, in
// TestRecvCarriesAuditText.
func drain(t *testing.T, s ChatStream) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for {
		payload, _, err := s.Recv()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("payload is not JSON: %v (%q)", err, payload)
		}
		out = append(out, m)
	}
}

func newPassthroughTestStream(body string) *passthroughStream {
	r := nopCloser{strings.NewReader(body)}
	return &passthroughStream{body: r, scanner: newStreamScanner(r)}
}

func TestPassthroughStreamUnwrapsSSEFrames(t *testing.T) {
	s := newPassthroughTestStream(`data: {"id":"a","choices":[{"index":0,"delta":{"content":"x"}}]}

: keepalive comment
event: ping
data: {"id":"b","choices":[{"index":0,"delta":{"content":"y"}}]}

data: [DONE]

`)
	chunks := drain(t, s)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2 (comments and event: lines must be dropped)", len(chunks))
	}
	if id, _ := chunks[0]["id"].(string); id != "a" {
		t.Errorf("first chunk id = %q, want \"a\"", id)
	}
	if id, _ := chunks[1]["id"].(string); id != "b" {
		t.Errorf("second chunk id = %q, want \"b\"", id)
	}
}

func TestPassthroughStreamEndsWithoutDone(t *testing.T) {
	// An upstream that just closes the connection must still terminate cleanly;
	// the handler supplies [DONE] either way.
	s := newPassthroughTestStream("data: {\"id\":\"a\"}\n")
	if n := len(drain(t, s)); n != 1 {
		t.Errorf("got %d chunks, want 1", n)
	}
}

func TestPassthroughStreamStopsAtDone(t *testing.T) {
	s := newPassthroughTestStream("data: {\"id\":\"a\"}\n\ndata: [DONE]\n\ndata: {\"id\":\"LEAKED\"}\n")
	chunks := drain(t, s)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1 — data after [DONE] leaked through", len(chunks))
	}
}

// Recv hands the handler the frame metadata alongside the payload, extracted
// in the same pass that read it: passthroughStream parses each frame once.
func TestRecvCarriesAuditText(t *testing.T) {
	passthrough := newPassthroughTestStream(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]}}]}

data: [DONE]

`)
	_, meta, err := passthrough.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	want := "\n[tool_call get_weather] {\"city\":\"Tokyo\"}"
	if meta.auditText != want {
		t.Errorf("passthrough audit text = %q, want %q", meta.auditText, want)
	}
}

// The usage block rides on the final chunk when the client asks for it
// (stream_options.include_usage); metaFromChunk must surface it.
func TestRecvCarriesUsage(t *testing.T) {
	s := newPassthroughTestStream(`data: {"model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5}}

data: [DONE]

`)
	var last chunkMeta
	for {
		_, meta, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		last = meta
	}
	if last.inTokens != 11 || last.outTokens != 5 {
		t.Errorf("usage = %d/%d, want 11/5", last.inTokens, last.outTokens)
	}
	if last.finish != "stop" {
		t.Errorf("finish = %q, want \"stop\"", last.finish)
	}
	if last.model != "gpt-x" {
		t.Errorf("model = %q, want \"gpt-x\"", last.model)
	}
}
