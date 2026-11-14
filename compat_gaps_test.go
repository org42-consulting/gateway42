package main

// The compatibility gaps closed together: engine errors reach the client as
// what they are, /v1/messages fails over like /v1/chat/completions, tool
// results keep their images, reasoning becomes thinking blocks, and document
// blocks reach the model.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── 1. Engine errors ─────────────────────────────────────────────────────────

func TestEngineFailureMapping(t *testing.T) {
	cases := []struct {
		err      error
		status   int
		errType  string
		contains string
	}{
		{&upstreamStatusError{400, "Prompt is too long: 9000 tokens > 8192 maximum context length"}, 400, "invalid_request_error", "Prompt is too long"},
		{&upstreamStatusError{422, "tools.0: invalid schema"}, 400, "invalid_request_error", "invalid schema"},
		{&upstreamStatusError{404, "model 'x' not found"}, 404, "invalid_request_error", "not found"},
		{&upstreamStatusError{413, ""}, 413, "invalid_request_error", "too large"},
		{&upstreamStatusError{429, "slow down"}, 429, "rate_limit_error", "slow down"},
		// The gateway's engine credentials, not the caller's: never passed on.
		{&upstreamStatusError{401, "invalid api key sk-engine-secret"}, 502, "api_error", "Could not reach engine"},
		{&upstreamStatusError{500, "CUDA out of memory"}, 502, "api_error", "Could not reach engine"},
		{errors.New("dial tcp: connection refused"), 502, "api_error", "Could not reach engine"},
	}
	for _, c := range cases {
		status, msg, typ := engineFailure(c.err)
		if status != c.status || typ != c.errType || !strings.Contains(msg, c.contains) {
			t.Errorf("engineFailure(%v) = %d %q %q, want %d %q containing %q", c.err, status, typ, msg, c.status, c.errType, c.contains)
		}
		if strings.Contains(msg, "sk-engine-secret") {
			t.Errorf("engine credential detail leaked: %q", msg)
		}
	}
}

func TestAnthropicContextOverflowWording(t *testing.T) {
	for in, want := range map[string]string{
		// oMLX already words it like Anthropic: left alone.
		"Prompt is too long: 9000 tokens > 8192 maximum context length": "Prompt is too long: 9000 tokens > 8192 maximum context length",
		// vLLM / llama.cpp phrasing gets the phrase Claude clients match.
		"This model's maximum context length is 8192 tokens. However, you requested 9000 tokens.": "prompt is too long: This model's maximum context length is 8192 tokens. However, you requested 9000 tokens.",
		"the request exceeds the available context size":                                          "the request exceeds the available context size", // not recognisably overflow: untouched
	} {
		rec := httptest.NewRecorder()
		writeAnthropicEngineFailure(rec, &upstreamStatusError{400, in})
		var body AnthropicErrorResponse
		json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 400 || body.Error.Message != want || body.Error.Type != "invalid_request_error" {
			t.Errorf("%q → %d %+v, want 400 %q", in, rec.Code, body.Error, want)
		}
	}
	rec := httptest.NewRecorder()
	writeAnthropicEngineFailure(rec, &upstreamStatusError{413, "too big"})
	if !strings.Contains(rec.Body.String(), `"type":"request_too_large"`) {
		t.Errorf("413 type: %s", rec.Body.String())
	}
}

func TestUpstreamErrMessageShapes(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"message":"bad tool","type":"invalid_request_error"}}`: "bad tool",                          // OpenAI-style
		`{"error":"model not found"}`:                                     "model not found",                   // Ollama
		`{"detail":"Prompt is too long: 10 tokens > 5"}`:                  "Prompt is too long: 10 tokens > 5", // FastAPI
		"plain text\nerror":                                               "plain text error",
	} {
		if got := upstreamErrMessage(strings.NewReader(body)); got != want {
			t.Errorf("upstreamErrMessage(%s) = %q, want %q", body, got, want)
		}
	}
}

// End to end: a real OpenAI-compatible adapter against an engine that rejects
// the prompt, through the full router, streaming and not, on both APIs.
func TestEngineRejectionReachesClientEndToEnd(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 4096)
	foSeedUser(t, "gap-user", "sk-gw-gap", 600)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"BadRequestError"}}`))
	}))
	defer engine.Close()
	ec := EngineConfig{ID: 1, Name: "gap", Type: EngineOpenAICompat, BaseURL: engine.URL}
	engineCachePtr.Store(&engineCache{
		engines:  []EngineConfig{ec},
		adapters: map[int]Engine{1: &OpenAICompatAdapter{cfg: ec, concurrencyLimiter: newConcurrencyLimiter(2)}},
	})
	storeRouteIndex(&routeIndex{byModel: map[string][]int{"m": {1}}, built: time.Now(), probed: true}, routeIndexGeneration())
	t.Cleanup(invalidateRouteIndex)

	for _, c := range []struct{ path, body, want string }{
		{"/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, `"message":"This model's maximum context length is 8192 tokens."`},
		{"/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, `"type":"invalid_request_error"`},
		{"/v1/messages", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, `"message":"prompt is too long: This model's maximum context length is 8192 tokens."`},
		{"/v1/messages", `{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, `"message":"prompt is too long: `},
	} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		r.Header.Set("Authorization", "Bearer sk-gw-gap")
		setupRouter().ServeHTTP(rec, r)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s %s → %d %s, want 400 with %s", c.path, c.body, rec.Code, rec.Body.String(), c.want)
		}
	}
}

// ── 3. Failover on /v1/messages ──────────────────────────────────────────────

func TestMessagesFailsOverWhenPrimaryIsBusy(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	foSeedUser(t, "gap-fo", "sk-gw-gap-fo", 60)

	primary, failover := foSeedPair(t, "fo-model")
	if !primary.TryAcquire(context.Background()) {
		t.Fatal("could not pre-acquire the primary's slot")
	}
	defer primary.Release()

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"fo-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("x-api-key", "sk-gw-gap-fo")
	setupRouter().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200 from the failover engine", rec.Code, rec.Body.String())
	}
	if len(primary.calls) != 0 || len(failover.calls) != 1 {
		t.Fatalf("calls: primary %v, failover %v; want the failover to serve", primary.calls, failover.calls)
	}
}

func TestMessagesAllBusyIs529Overloaded(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	foSeedUser(t, "gap-busy", "sk-gw-gap-busy", 60)

	primary, failover := foSeedPair(t, "fo-model")
	if !primary.TryAcquire(context.Background()) || !failover.TryAcquire(context.Background()) {
		t.Fatal("could not pre-acquire both slots")
	}
	defer primary.Release()
	defer failover.Release()

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"fo-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("x-api-key", "sk-gw-gap-busy")
	setupRouter().ServeHTTP(rec, r)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "2" || !strings.Contains(rec.Body.String(), "overloaded_error") {
		t.Fatalf("got %d %q %s, want 503 overloaded_error with Retry-After", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

// ── 2. Images in tool results ────────────────────────────────────────────────

func TestToolResultImagesFollowToolMessages(t *testing.T) {
	body := decodeJSON(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"toolu_1","content":[
			{"type":"text","text":"Screenshot taken"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]},
		{"type":"tool_result","tool_use_id":"toolu_2","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/"}}]}
	]}]}`).(map[string]interface{})
	_, msgs := anthropicToOpenAI(body)
	got, _ := json.Marshal(msgs)
	want := `[{"content":"Screenshot taken","role":"tool","tool_call_id":"toolu_1"},` +
		`{"content":"[The tool returned images; they follow in the next message.]","role":"tool","tool_call_id":"toolu_2"},` +
		`{"content":[{"text":"[Image output of tool call toolu_1]","type":"text"},` +
		`{"image_url":{"url":"data:image/png;base64,iVBOR"},"type":"image_url"},` +
		`{"text":"[Image output of tool call toolu_2]","type":"text"},` +
		`{"image_url":{"url":"data:image/jpeg;base64,/9j/"},"type":"image_url"}],"role":"user"}]`
	if string(got) != want {
		t.Fatalf("messages:\n got %s\nwant %s", got, want)
	}

	// Ollama receives them as the user message's images.
	om := ollamaMessages(sanitizeMessages(msgs))
	if imgs, _ := om[2]["images"].([]string); len(imgs) != 2 {
		t.Fatalf("Ollama images = %v, want both tool images", om[2]["images"])
	}
}

func TestTextOnlyToolResultUnchanged(t *testing.T) {
	body := decodeJSON(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t","content":"ok"}]}]}`).(map[string]interface{})
	_, msgs := anthropicToOpenAI(body)
	got, _ := json.Marshal(msgs)
	if string(got) != `[{"content":"ok","role":"tool","tool_call_id":"t"}]` {
		t.Fatalf("text-only tool result changed: %s", got)
	}
}

// ── 4. Reasoning → thinking ──────────────────────────────────────────────────

func streamEvents(t *testing.T, thinking bool, chunks ...string) string {
	t.Helper()
	s := newAnthropicStream("m")
	s.thinking = thinking
	evs := []sseEvent{s.start()}
	for _, c := range chunks {
		evs = append(evs, s.chunk([]byte(c))...)
	}
	evs = append(evs, s.end()...)
	var b strings.Builder
	for _, ev := range evs {
		d, _ := json.Marshal(ev.data)
		b.WriteString(ev.name + " " + string(d) + "\n")
	}
	return b.String()
}

func TestStreamReasoningBecomesThinkingBlock(t *testing.T) {
	chunks := []string{
		`{"choices":[{"delta":{"reasoning_content":"Let me "}}]}`,
		`{"choices":[{"delta":{"reasoning":"think."}}]}`, // vLLM's name
		`{"choices":[{"delta":{"content":"Answer"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
	got := streamEvents(t, true, chunks...)
	for _, want := range []string{
		`content_block_start {"content_block":{"type":"thinking","thinking":"","signature":""},"index":0,"type":"content_block_start"}`,
		`{"delta":{"thinking":"Let me ","type":"thinking_delta"},"index":0`,
		`{"delta":{"thinking":"think.","type":"thinking_delta"},"index":0`,
		`content_block_stop {"index":0`,
		`content_block_start {"content_block":{"type":"text","text":""},"index":1`,
		`{"delta":{"text":"Answer","type":"text_delta"},"index":1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %s\n%s", want, got)
		}
	}

	// Not requested: reasoning is dropped and the answer is block 0, as before.
	got = streamEvents(t, false, chunks...)
	if strings.Contains(got, "thinking") || !strings.Contains(got, `"text":"Answer","type":"text_delta"},"index":0`) {
		t.Errorf("unrequested reasoning leaked or shifted indices:\n%s", got)
	}
}

func TestNonStreamReasoningBecomesThinkingBlock(t *testing.T) {
	payload := decodeJSON(t, `{"choices":[{"index":0,"finish_reason":"stop","message":{
		"role":"assistant","content":"42","reasoning_content":"6 times 7"}}]}`)
	msg, err := openAIToAnthropic(payload, "m")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(msg.Content)
	if string(got) != `[{"type":"thinking","thinking":"6 times 7","signature":""},{"type":"text","text":"42"}]` {
		t.Fatalf("content = %s", got)
	}
	withoutThinking(msg)
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Fatalf("withoutThinking left %+v", msg.Content)
	}
}

func TestThinkingRequested(t *testing.T) {
	for body, want := range map[string]bool{
		`{"thinking":{"type":"enabled","budget_tokens":2048}}`: true,
		`{"thinking":{"type":"adaptive"}}`:                     true,
		`{"thinking":{"type":"disabled"}}`:                     false,
		`{}`:                                                   false,
	} {
		if got := thinkingRequested(decodeJSON(t, body).(map[string]interface{})); got != want {
			t.Errorf("thinkingRequested(%s) = %v", body, got)
		}
	}
}

func TestOllamaThinkingBecomesReasoningContent(t *testing.T) {
	cc := ollamaToOpenAI(map[string]interface{}{"done": true,
		"message": map[string]interface{}{"role": "assistant", "content": "hi", "thinking": "greet"}})
	if cc.Choices[0].Message.ReasoningContent != "greet" {
		t.Fatalf("message = %+v", cc.Choices[0].Message)
	}
	chunk := formatStreamChunk(map[string]interface{}{"message": map[string]interface{}{"thinking": "hm"}}, "id", false)
	if chunk.Choices[0].Delta.ReasoningContent != "hm" {
		t.Fatalf("delta = %+v", chunk.Choices[0].Delta)
	}
	// No thinking, no new field: authored bodies are unchanged.
	b, _ := json.Marshal(ollamaToOpenAI(map[string]interface{}{"done": true,
		"message": map[string]interface{}{"role": "assistant", "content": "hi"}}))
	if strings.Contains(string(b), "reasoning") {
		t.Fatalf("reasoning field without reasoning: %s", b)
	}
}

func TestReasoningParamsForwarded(t *testing.T) {
	data := decodeJSON(t, `{"model":"m","reasoning_effort":"low","chat_template_kwargs":{"enable_thinking":false}}`).(map[string]interface{})
	b, _ := json.Marshal(openAICompatRequest(data, nil))
	for _, want := range []string{`"reasoning_effort":"low"`, `"chat_template_kwargs":{"enable_thinking":false}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("request missing %s: %s", want, b)
		}
	}
}

// ── 5. Documents ─────────────────────────────────────────────────────────────

func TestDocumentBlocksReachTheModel(t *testing.T) {
	withMaxMsgLen(t, 4096)
	body := decodeJSON(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"document","title":"notes.txt","context":"Meeting notes","source":{"type":"text","media_type":"text/plain","data":"Ship on Friday."}},
		{"type":"document","source":{"type":"content","content":[{"type":"text","text":"Part one."},{"type":"text","text":"Part two."}]}},
		{"type":"document","title":"spec.pdf","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}},
		{"type":"text","text":"Summarise."}
	]}]}`).(map[string]interface{})
	_, msgs := anthropicToOpenAI(body)
	content := sanitizeMessages(msgs)[0]["content"].(string)
	for _, want := range []string{
		"Document: notes.txt\nMeeting notes\n\nShip on Friday.",
		"Part one.\n\nPart two.",
		`[Attached application/pdf "spec.pdf" could not be included: this model cannot read that format.]`,
		"Summarise.",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "JVBERi0x") {
		t.Errorf("raw PDF bytes reached the prompt")
	}
}

func TestDocumentInToolResult(t *testing.T) {
	body := decodeJSON(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t","content":[
			{"type":"document","title":"README","source":{"type":"text","media_type":"text/plain","data":"Hello"}}]}]}]}`).(map[string]interface{})
	_, msgs := anthropicToOpenAI(body)
	if c := msgs[0].(map[string]interface{})["content"]; c != "Document: README\n\nHello" {
		t.Fatalf("tool content = %q", c)
	}
}
