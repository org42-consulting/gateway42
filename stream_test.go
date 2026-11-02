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

func newOllamaTestStream(body string) *ollamaStream {
	r := nopCloser{strings.NewReader(body)}
	return &ollamaStream{
		body:         r,
		scanner:      newStreamScanner(r),
		completionID: "chatcmpl-test",
		isFirst:      true,
	}
}

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

// delta pulls choices[0].delta out of a chunk.
func delta(t *testing.T, chunk map[string]interface{}) map[string]interface{} {
	t.Helper()
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("chunk has no choices: %v", chunk)
	}
	c, _ := choices[0].(map[string]interface{})
	d, _ := c["delta"].(map[string]interface{})
	return d
}

func TestOllamaStreamTranslatesToOpenAIChunks(t *testing.T) {
	s := newOllamaTestStream(`{"model":"llama3","message":{"role":"assistant","content":"Hel"},"done":false}
{"model":"llama3","message":{"role":"assistant","content":"lo"},"done":false}
{"model":"llama3","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":7,"eval_count":2}
`)
	chunks := drain(t, s)

	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}

	// Only the first delta carries the role: OpenAI clients accumulate content
	// and would otherwise see the role repeated on every token.
	if role, _ := delta(t, chunks[0])["role"].(string); role != "assistant" {
		t.Errorf("first chunk role = %q, want \"assistant\"", role)
	}
	if _, present := delta(t, chunks[1])["role"]; present {
		t.Error("second chunk should not repeat the role")
	}

	var text strings.Builder
	for _, c := range chunks {
		if s, _ := delta(t, c)["content"].(string); s != "" {
			text.WriteString(s)
		}
	}
	if text.String() != "Hello" {
		t.Errorf("reassembled content = %q, want \"Hello\"", text.String())
	}

	// The final Ollama object carries done:true *and* the token counts, so the
	// translator must emit it rather than treating done as end-of-stream.
	last := chunks[2]
	usage, ok := last["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("final chunk has no usage: %v", last)
	}
	if total, _ := usage["total_tokens"].(float64); total != 9 {
		t.Errorf("total_tokens = %v, want 9", total)
	}
	choices, _ := last["choices"].([]interface{})
	c0, _ := choices[0].(map[string]interface{})
	if fr, _ := c0["finish_reason"].(string); fr != "stop" {
		t.Errorf("finish_reason = %q, want \"stop\"", fr)
	}
}

func TestOllamaStreamStopsAfterDone(t *testing.T) {
	// Anything after done:true is upstream noise and must not reach the client.
	s := newOllamaTestStream(`{"model":"llama3","message":{"content":"hi"},"done":true}
{"model":"llama3","message":{"content":"LEAKED"},"done":false}
`)
	if n := len(drain(t, s)); n != 1 {
		t.Errorf("got %d chunks, want 1 — content after done: leaked through", n)
	}
}

func TestOllamaStreamSkipsMalformedLines(t *testing.T) {
	// A single bad line should not truncate an otherwise healthy completion.
	s := newOllamaTestStream(`{"model":"llama3","message":{"content":"a"},"done":false}
not json at all
{"model":"llama3","message":{"content":"b"},"done":true}
`)
	if n := len(drain(t, s)); n != 2 {
		t.Errorf("got %d chunks, want 2", n)
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
// in the same pass that shaped it. These pin both extraction paths:
// ollamaStream reads its typed chunk, passthroughStream parses the frame once.
func TestRecvCarriesAuditText(t *testing.T) {
	ollama := newOllamaTestStream(`{"model":"llama3","message":{"role":"assistant","content":"Hel"},"done":false}
{"model":"llama3","message":{"content":"lo"},"done":false}
{"model":"llama3","message":{"content":""},"eval_count":7,"prompt_eval_count":3,"done":true}
`)
	var text strings.Builder
	var last chunkMeta
	for {
		_, meta, err := ollama.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		text.WriteString(meta.auditText)
		last = meta
	}
	if text.String() != "Hello" {
		t.Errorf("ollama audit text = %q, want \"Hello\"", text.String())
	}
	// The terminal chunk carries the finish reason and Ollama's eval counts,
	// which the request log stores as stop_reason and token usage.
	if last.finish != "stop" {
		t.Errorf("ollama finish = %q, want \"stop\"", last.finish)
	}
	if last.model != "llama3" {
		t.Errorf("ollama model = %q, want \"llama3\"", last.model)
	}
	if last.inTokens != 3 || last.outTokens != 7 {
		t.Errorf("ollama usage = %d/%d, want 3/7", last.inTokens, last.outTokens)
	}

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
