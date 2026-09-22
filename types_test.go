package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests assert on the *encoded* shape rather than on struct fields,
// because the whole point of the tags is what reaches the client. A key that
// should be present-but-null and a key that should be absent are different
// bugs, and only the JSON tells them apart.

// keys returns the top-level object keys present in the encoding of v.
func keys(t *testing.T, v interface{}) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func TestChunkOmitsUsageUntilFinal(t *testing.T) {
	mid := formatStreamChunk(
		map[string]interface{}{"model": "m", "message": map[string]interface{}{"content": "x"}},
		"id", false)
	if keys(t, mid)["usage"] {
		t.Error("mid-stream chunk carries usage; clients that sum usage would over-count")
	}

	final := formatStreamChunk(
		map[string]interface{}{"model": "m", "done": true, "prompt_eval_count": 3, "eval_count": 4},
		"id", false)
	if !keys(t, final)["usage"] {
		t.Error("final chunk is missing usage")
	}
}

func TestChunkFinishReasonIsNullNotAbsent(t *testing.T) {
	// OpenAI clients read choices[0].finish_reason and check it against null.
	// Omitting the key makes some of them fault on a missing field, so it must
	// be present and null mid-stream.
	mid := formatStreamChunk(
		map[string]interface{}{"model": "m", "message": map[string]interface{}{"content": "x"}},
		"id", false)
	b, err := json.Marshal(mid)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"finish_reason":null`) {
		t.Errorf("mid-stream finish_reason should be present and null, got: %s", b)
	}
}

func TestFirstChunkCarriesRoleAndLaterOnesDoNot(t *testing.T) {
	ollamaLine := map[string]interface{}{
		"model": "m", "message": map[string]interface{}{"content": "x"},
	}

	first, err := json.Marshal(formatStreamChunk(ollamaLine, "id", true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(first), `"role":"assistant"`) {
		t.Errorf("first chunk should declare the role, got: %s", first)
	}

	later, err := json.Marshal(formatStreamChunk(ollamaLine, "id", false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(later), `"role"`) {
		t.Errorf("later chunks must not repeat the role, got: %s", later)
	}
}

func TestErrorResponseKeepsNullCode(t *testing.T) {
	b, err := json.Marshal(openaiError("boom", "api_error"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"code":null`) {
		t.Errorf("error code should be present and null (SDKs read it), got: %s", got)
	}
	if !strings.Contains(got, `"error":{`) {
		t.Errorf("error must be wrapped in the OpenAI envelope, got: %s", got)
	}
}

func TestModelOmitsSizeWhenUnknown(t *testing.T) {
	// size is a Gateway42 extension. OpenAI-compatible engines do not report
	// it, and emitting size:0 would have clients render "0 B" as if it were a
	// measurement.
	if keys(t, Model{ID: "m", Object: "model", OwnedBy: "openai_compat"})["size"] {
		t.Error("size should be omitted when zero")
	}
	if !keys(t, Model{ID: "m", Object: "model", OwnedBy: "ollama", Size: 42})["size"] {
		t.Error("size should be emitted when known")
	}
}

func TestCompletionUsageAlwaysPresent(t *testing.T) {
	// A completion that used zero measured tokens must still report usage:
	// absence means "not measured", zero means "measured as zero".
	c := ollamaToOpenAI(map[string]interface{}{
		"model":   "m",
		"message": map[string]interface{}{"role": "assistant", "content": "hi"},
	})
	if !keys(t, c)["usage"] {
		t.Error("completion is missing usage")
	}
	if c.Choices[0].FinishReason == nil || *c.Choices[0].FinishReason != "stop" {
		t.Error("completed response should finish with \"stop\"")
	}
}

func TestOllamaToOpenAIDefaultsRoleWhenUpstreamOmitsIt(t *testing.T) {
	// Some Ollama builds omit message.role. An empty role makes clients drop
	// the message, so it defaults to assistant.
	c := ollamaToOpenAI(map[string]interface{}{
		"model":   "m",
		"message": map[string]interface{}{"content": "hi"},
	})
	if c.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want \"assistant\"", c.Choices[0].Message.Role)
	}
}
