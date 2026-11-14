package main

import (
	"encoding/json"
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

func withMaxInputLen(t *testing.T, n int) {
	t.Helper()
	prev := cfg.MaxInputLen
	cfg.MaxInputLen = n
	t.Cleanup(func() { cfg.MaxInputLen = prev })
}

// MAX_MESSAGE_LENGTH bounds the audit log, not the model's input: by default a
// message reaches the engine whole, however long.
func TestSanitizeMessagesKeepsLongTextByDefault(t *testing.T) {
	withMaxMsgLen(t, 5)
	withMaxInputLen(t, 0)
	msgs := sanitizeMessages(decodeJSON(t, `[{"role":"user","content":"0123456789"}]`).([]interface{}))
	if msgs[0]["content"] != "0123456789" {
		t.Fatalf("content = %q, want it whole", msgs[0]["content"])
	}
}

func TestSanitizeMessagesTruncatesArrayTextWithoutMutatingInput(t *testing.T) {
	withMaxInputLen(t, 5)
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
