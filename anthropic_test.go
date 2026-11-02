package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicToOpenAITranslatesToolLoop(t *testing.T) {
	body := decodeJSON(t, `{
		"model": "qwen3",
		"max_tokens": 1024,
		"stream": true,
		"stop_sequences": ["END"],
		"system": [{"type":"text","text":"You are terse.","cache_control":{"type":"ephemeral"}}],
		"tools": [
			{"name":"Read","description":"Read a file","input_schema":{"type":"object"}},
			{"type":"web_search_20250305","name":"web_search"}
		],
		"tool_choice": {"type":"any"},
		"messages": [
			{"role":"user","content":"read a.txt"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"hmm"},
				{"type":"text","text":"Reading."},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"a.txt"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"hello"}]},
				{"type":"text","text":"now summarise"}
			]}
		]
	}`).(map[string]interface{})

	data, msgs := anthropicToOpenAI(body)

	if data["max_tokens"] != float64(1024) || data["stream"] != true {
		t.Errorf("params not carried over: %v", data)
	}
	if stop, _ := data["stop"].([]interface{}); len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v, want [END]", data["stop"])
	}
	// The server tool has no input_schema and must not be offered as a function.
	if tools, _ := data["tools"].([]interface{}); len(tools) != 1 {
		t.Fatalf("tools = %v, want only the client tool", data["tools"])
	}
	if data["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v, want required", data["tool_choice"])
	}

	got, _ := json.Marshal(msgs)
	want := `[{"content":"You are terse.","role":"system"},` +
		`{"content":"read a.txt","role":"user"},` +
		`{"content":"Reading.","role":"assistant","tool_calls":[{"function":{"arguments":"{\"path\":\"a.txt\"}","name":"Read"},"id":"toolu_1","type":"function"}]},` +
		// The tool result comes before the user's text: OpenAI needs it
		// directly after the assistant turn that made the call.
		`{"content":"hello","role":"tool","tool_call_id":"toolu_1"},` +
		`{"content":"now summarise","role":"user"}]`
	if string(got) != want {
		t.Errorf("messages:\n got %s\nwant %s", got, want)
	}
}

func TestAnthropicToOpenAIToolOnlyTurnHasNullContent(t *testing.T) {
	body := decodeJSON(t, `{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Bash","input":{}}]}
	]}`).(map[string]interface{})
	_, msgs := anthropicToOpenAI(body)
	clean := sanitizeMessages(msgs)
	if len(clean) != 1 || clean[0]["content"] != nil {
		t.Fatalf("tool-only assistant turn = %v, want null content", clean)
	}
}

func TestOpenAIToAnthropicNonStreaming(t *testing.T) {
	payload := decodeJSON(t, `{"choices":[{"index":0,"finish_reason":"stop","message":{
		"role":"assistant","content":"Let me look.",
		"tool_calls":[{"id":"call_9","type":"function","function":{"name":"Read","arguments":"{\"path\":\"x\"}"}}]
	}}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)

	msg, err := openAIToAnthropic(payload, "qwen3")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(msg)
	s := string(got)
	for _, want := range []string{
		`"type":"message"`, `"model":"qwen3"`,
		`{"type":"text","text":"Let me look."}`,
		`{"type":"tool_use","id":"call_9","name":"Read","input":{"path":"x"}}`,
		// An engine that said "stop" after calling tools still means tool_use.
		`"stop_reason":"tool_use"`, `"stop_sequence":null`,
		`"usage":{"input_tokens":11,"output_tokens":4}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("response missing %s\n%s", want, s)
		}
	}
}

func TestOpenAIToAnthropicLengthAndBadArguments(t *testing.T) {
	msg, err := openAIToAnthropic(&ChatCompletion{Choices: []Choice{{
		Message: Message{ToolCalls: []ToolCall{{Function: FunctionCall{Name: "f", Arguments: "{broken"}}}},
	}, {}}}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if string(msg.Content[0].Input) != "{}" || msg.Content[0].ID == "" {
		t.Errorf("bad arguments should become {} with a generated id: %+v", msg.Content[0])
	}

	msg, _ = openAIToAnthropic(&ChatCompletion{Choices: []Choice{{
		Message: Message{Content: "cut"}, FinishReason: &finishLength,
	}}}, "m")
	if *msg.StopReason != "max_tokens" {
		t.Errorf("stop_reason = %s, want max_tokens", *msg.StopReason)
	}
}

// eventNames renders a stream as "name:index" (index only where present) so
// block sequencing reads at a glance.
func eventNames(events []sseEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		name := ev.name
		if m, ok := ev.data.(map[string]interface{}); ok {
			if i, ok := m["index"].(int); ok {
				name += ":" + string(rune('0'+i))
			}
		}
		out = append(out, name)
	}
	return out
}

func TestAnthropicStreamSequencesBlocks(t *testing.T) {
	s := newAnthropicStream("m")
	events := []sseEvent{s.start()}
	for _, chunk := range []string{
		`{"choices":[{"delta":{"role":"assistant","content":"Hi"}}]}`,
		`{"choices":[{"delta":{"content":" there"}}]}`,
		// OpenAI streams a call as a header fragment then argument fragments.
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"Read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"Bash","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`,
		`not json`,
	} {
		events = append(events, s.chunk([]byte(chunk))...)
	}
	events = append(events, s.end()...)

	want := []string{
		"message_start",
		"content_block_start:0", "content_block_delta:0", "content_block_delta:0",
		"content_block_stop:0",
		"content_block_start:1", "content_block_delta:1", "content_block_delta:1",
		"content_block_stop:1",
		"content_block_start:2", "content_block_delta:2",
		"content_block_stop:2",
		"message_delta", "message_stop",
	}
	if got := eventNames(events); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}

	b, _ := json.Marshal(events[len(events)-2].data)
	if !strings.Contains(string(b), `"stop_reason":"tool_use"`) ||
		!strings.Contains(string(b), `"output_tokens":9`) {
		t.Errorf("message_delta = %s", b)
	}
	b, _ = json.Marshal(events[5].data)
	if !strings.Contains(string(b), `"id":"call_a","name":"Read","input":{}`) {
		t.Errorf("tool block start = %s", b)
	}
}

// End to end through the real Ollama translator: an Ollama tool-calling reply
// becomes a tool_use block with the whole arguments in one delta.
func TestAnthropicStreamOverOllama(t *testing.T) {
	src := newOllamaTestStream(`{"model":"q","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"Read","arguments":{"path":"a"}}}]},"done":false}
{"model":"q","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":3,"eval_count":2}
`)
	s := newAnthropicStream("q")
	var events []sseEvent
	for _, payload := range drain(t, src) {
		b, _ := json.Marshal(payload)
		events = append(events, s.chunk(b)...)
	}
	events = append(events, s.end()...)

	var all strings.Builder
	for _, ev := range events {
		b, _ := json.Marshal(ev.data)
		all.Write(b)
	}
	for _, want := range []string{
		`"type":"tool_use"`, `"name":"Read"`,
		`"partial_json":"{\"path\":\"a\"}"`,
		`"stop_reason":"tool_use"`, `"input_tokens":3`,
	} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("stream missing %s\n%s", want, all.String())
		}
	}
}

func TestAnthropicErrorTypes(t *testing.T) {
	for status, want := range map[int]string{
		404: "not_found_error", 503: "overloaded_error", 429: "rate_limit_error", 400: "invalid_request_error",
	} {
		in := map[int]string{429: "rate_limit_error", 400: "invalid_request_error"}[status]
		if got := anthropicError(status, "x", in).Error.Type; got != want {
			t.Errorf("status %d: type = %s, want %s", status, got, want)
		}
	}
}

// Claude Code reads error.type from the Anthropic envelope; an OpenAI-shaped
// 401 on /v1/messages left it unable to tell a bad key from a server fault.
func TestAuthErrorShapeFollowsPath(t *testing.T) {
	h := apiAuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler reached without a key")
	}))
	for path, want := range map[string]string{
		"/v1/messages":              `{"type":"error","error":{"type":"authentication_error"`,
		"/v1/messages/count_tokens": `{"type":"error","error":{"type":"authentication_error"`,
		"/v1/chat/completions":      `{"error":{"message":"Missing API key","type":"authentication_error"`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		if rec.Code != 401 || !strings.HasPrefix(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestMessagesRouteRegistered(t *testing.T) {
	r := setupRouter()
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader("{}")))
		// Before this route existed the CORS preflight route made mux answer
		// 405 with an empty body; now auth runs and rejects the missing key.
		if rec.Code != 401 {
			t.Errorf("POST %s = %d, want 401", path, rec.Code)
		}
	}
}
