package main

import (
	"encoding/json"
	"gateway42/internal/wire"
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
	if keys(t, wire.Model{ID: "m", Object: "model", OwnedBy: "openai_compat"})["size"] {
		t.Error("size should be omitted when zero")
	}
	if !keys(t, wire.Model{ID: "m", Object: "model", OwnedBy: "omlx", Size: 42})["size"] {
		t.Error("size should be emitted when known")
	}
}
