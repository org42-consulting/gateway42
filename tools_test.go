package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Tool use is a loop: the model calls a tool, the client runs it, and the next
// request carries the whole history back — the assistant turn with its
// tool_calls and a tool turn with the result. Each test below covers one leg
// of that loop; any one of them breaking makes the second request fail.

func withMaxMsgLen(t *testing.T, n int) {
	t.Helper()
	prev := cfg.MaxMsgLen
	cfg.MaxMsgLen = n
	t.Cleanup(func() { cfg.MaxMsgLen = prev })
}

// decodeJSON round-trips a literal so tests see exactly what the request
// decoder produces: []interface{} and map[string]interface{}, float64 numbers.
func decodeJSON(t *testing.T, s string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return v
}

const toolLoopHistory = `[
  {"role":"user","content":"Weather in Tokyo?"},
  {"role":"assistant","content":null,"tool_calls":[
    {"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}
  ]},
  {"role":"tool","tool_call_id":"call_abc","content":"22C, clear"}
]`

func TestSanitizeMessagesKeepsToolFields(t *testing.T) {
	withMaxMsgLen(t, 1000)
	msgs := sanitizeMessages(decodeJSON(t, toolLoopHistory).([]interface{}))

	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if _, ok := msgs[1]["tool_calls"]; !ok {
		t.Error("assistant turn lost its tool_calls; the upstream cannot match the result to a call")
	}
	if msgs[1]["content"] != nil {
		t.Errorf("tool-calling assistant content = %#v, want null", msgs[1]["content"])
	}
	if msgs[2]["tool_call_id"] != "call_abc" {
		t.Errorf("tool turn tool_call_id = %#v, want call_abc", msgs[2]["tool_call_id"])
	}
}

func TestSanitizeMessagesAbsentContentStaysEmptyString(t *testing.T) {
	withMaxMsgLen(t, 1000)
	msgs := sanitizeMessages(decodeJSON(t, `[{"role":"user"}]`).([]interface{}))
	if msgs[0]["content"] != "" {
		t.Errorf("content = %#v, want \"\" for a non-tool message", msgs[0]["content"])
	}
}

func TestSanitizeMessagesTruncatesArrayTextWithoutMutatingInput(t *testing.T) {
	withMaxMsgLen(t, 5)
	raw := decodeJSON(t, `[{"role":"user","content":[
		{"type":"text","text":"0123456789"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
	]}]`).([]interface{})

	msgs := sanitizeMessages(raw)
	parts, ok := msgs[0]["content"].([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want both parts preserved", msgs[0]["content"])
	}
	if got := parts[0].(map[string]interface{})["text"]; got != "01234" {
		t.Errorf("text part = %q, want truncated to \"01234\"", got)
	}

	orig := raw[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["text"]
	if orig != "0123456789" {
		t.Errorf("caller's request was modified: %q", orig)
	}
}

func TestOllamaFormat(t *testing.T) {
	schema := map[string]interface{}{"type": "object"}
	for _, tc := range []struct {
		name string
		in   interface{}
		want interface{}
	}{
		{"absent", nil, nil},
		{"text", map[string]interface{}{"type": "text"}, nil},
		{"json mode", map[string]interface{}{"type": "json_object"}, "json"},
		{"schema", map[string]interface{}{"type": "json_schema",
			"json_schema": map[string]interface{}{"name": "x", "schema": schema}}, schema},
		{"schema missing degrades to json", map[string]interface{}{"type": "json_schema"}, "json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ollamaFormat(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ollamaFormat(%v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestOpenAIToOllamaForwardsToolsAndFormat(t *testing.T) {
	data := decodeJSON(t, `{"model":"m",
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],
		"response_format":{"type":"json_object"}}`).(map[string]interface{})

	req := openAIToOllama(data, nil)
	if tools, _ := req["tools"].([]interface{}); len(tools) != 1 {
		t.Errorf("tools = %#v, want the one tool forwarded", req["tools"])
	}
	if req["format"] != "json" {
		t.Errorf("format = %#v, want \"json\"", req["format"])
	}
}

func TestOpenAIToOllamaHonoursToolChoiceNone(t *testing.T) {
	data := decodeJSON(t, `{"model":"m","tool_choice":"none",
		"tools":[{"type":"function","function":{"name":"f"}}]}`).(map[string]interface{})
	if _, sent := openAIToOllama(data, nil)["tools"]; sent {
		t.Error(`tool_choice "none" must withhold the tools; Ollama has no other way to express it`)
	}
}

func TestOllamaMessagesTranslatesToolLoop(t *testing.T) {
	withMaxMsgLen(t, 1000)
	msgs := ollamaMessages(sanitizeMessages(decodeJSON(t, toolLoopHistory).([]interface{})))

	calls, _ := msgs[1]["tool_calls"].([]map[string]interface{})
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %#v, want 1", msgs[1]["tool_calls"])
	}
	fn := calls[0]["function"].(map[string]interface{})
	args, isObject := fn["arguments"].(map[string]interface{})
	if !isObject || args["city"] != "Tokyo" {
		t.Errorf("arguments = %#v, want the decoded object Ollama expects", fn["arguments"])
	}
	if msgs[1]["content"] != "" {
		t.Errorf("null content should reach Ollama as \"\", got %#v", msgs[1]["content"])
	}
	if msgs[2]["tool_name"] != "get_weather" {
		t.Errorf("tool_name = %#v, want get_weather recovered from tool_call_id", msgs[2]["tool_name"])
	}
}

func TestOllamaMessagesFlattensContentParts(t *testing.T) {
	msgs := ollamaMessages([]map[string]interface{}{{
		"role": "user",
		"content": decodeJSON(t, `[
			{"type":"text","text":"What is"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
			{"type":"image_url","image_url":{"url":"https://example.com/remote.png"}},
			{"type":"text","text":"in this picture?"}
		]`),
	}})

	if msgs[0]["content"] != "What is\nin this picture?" {
		t.Errorf("content = %q, want the text parts joined", msgs[0]["content"])
	}
	images, _ := msgs[0]["images"].([]string)
	if !reflect.DeepEqual(images, []string{"QUJD"}) {
		t.Errorf("images = %#v, want only the inline base64 (remote URLs cannot be passed)", images)
	}
}

func TestOllamaToOpenAIReportsToolCalls(t *testing.T) {
	c := ollamaToOpenAI(decodeJSON(t, `{"model":"m","done":true,"done_reason":"stop",
		"message":{"role":"assistant","content":"",
		"tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Tokyo"}}}]}}`).(map[string]interface{}))

	choice := c.Choices[0]
	if choice.FinishReason == nil || *choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %#v, want 1", choice.Message.ToolCalls)
	}
	tc := choice.Message.ToolCalls[0]
	if tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v", tc)
	}
	if !strings.HasPrefix(tc.ID, "call_") {
		t.Errorf("ID = %q; the client must echo an ID back as tool_call_id", tc.ID)
	}
	// OpenAI clients json.loads the arguments, so they must be a string.
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil || args["city"] != "Tokyo" {
		t.Errorf("arguments = %q, want a JSON string encoding {city: Tokyo}", tc.Function.Arguments)
	}
}

func TestOllamaToOpenAIWithoutToolsHasNoToolCallsKey(t *testing.T) {
	c := ollamaToOpenAI(map[string]interface{}{
		"model": "m", "message": map[string]interface{}{"role": "assistant", "content": "hi"},
	})
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "tool_calls") {
		t.Errorf("plain reply carries a tool_calls key: %s", b)
	}
}

func TestOllamaStreamNumbersToolCallsAcrossChunks(t *testing.T) {
	s := newOllamaTestStream(`{"model":"m","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"a","arguments":{}}}]},"done":false}
{"model":"m","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"b","arguments":{"x":1}}}]},"done":false}
{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}
`)
	chunks := drain(t, s)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}

	for i, want := range []float64{0, 1} {
		calls, _ := delta(t, chunks[i])["tool_calls"].([]interface{})
		if len(calls) != 1 {
			t.Fatalf("chunk %d tool_calls = %#v, want 1", i, calls)
		}
		call := calls[0].(map[string]interface{})
		if call["index"] != want {
			t.Errorf("chunk %d index = %v, want %v; clients assemble calls by index", i, call["index"], want)
		}
		if _, isString := call["function"].(map[string]interface{})["arguments"].(string); !isString {
			t.Errorf("chunk %d arguments are not a JSON string", i)
		}
	}

	choices, _ := chunks[2]["choices"].([]interface{})
	if fr, _ := choices[0].(map[string]interface{})["finish_reason"].(string); fr != "tool_calls" {
		t.Errorf("final finish_reason = %q, want tool_calls (Ollama ends a tool call with \"stop\")", fr)
	}
}
