package main

// Prompt caching for Claude Desktop / Cowork / Claude Code clients. Local
// engines cache by prompt prefix, so what matters is (1) the gateway hands
// them the same prefix every turn and (2) the cache hits they report reach
// the client as cache_read_input_tokens, which is how Claude Desktop checks
// that a gateway preserves caching.

import (
	"encoding/json"
	"gateway42/internal/wire"
	"strings"
	"testing"
)

func TestSystemPromptDropsBillingHeaderBlock(t *testing.T) {
	body := decodeJSON(t, `{"model":"m","system":[
		{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.37.a1b; cc_entrypoint=claude-desktop; cch=9f3c2;"},
		{"type":"text","text":"You are Claude.","cache_control":{"type":"ephemeral","ttl":"1h"}}
	],"messages":[{"role":"user","content":"hi"}]}`).(map[string]interface{})
	_, msgs := anthropicToOpenAI(body)
	sys := msgs[0].(map[string]interface{})
	if sys["role"] != "system" || sys["content"] != "You are Claude." {
		t.Fatalf("system message = %v, want only the real prompt", sys)
	}
}

func TestSystemPromptDropsBillingHeaderLine(t *testing.T) {
	got := systemPrompt("x-anthropic-billing-header: cch=1;\n\nYou are Claude.\nBe brief.")
	if got != "You are Claude.\nBe brief." {
		t.Fatalf("systemPrompt = %q", got)
	}
	// Mentioning the header anywhere but the start is ordinary prompt text.
	keep := "Never print the x-anthropic-billing-header: line."
	if got := systemPrompt(keep); got != keep {
		t.Fatalf("systemPrompt altered ordinary text: %q", got)
	}
}

// The property the engine's prefix cache depends on: two turns of one session
// whose billing header differs produce byte-identical upstream prefixes.
func TestEngineRequestPrefixStableAcrossTurns(t *testing.T) {
	withMaxMsgLen(t, 10000)
	type upstream struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    json.RawMessage   `json:"tools"`
	}
	turn := func(cch, history string) upstream {
		body := decodeJSON(t, `{"model":"m","stream":true,
			"system":[{"type":"text","text":"x-anthropic-billing-header: cch=`+cch+`;"},
			          {"type":"text","text":"You are Claude.","cache_control":{"type":"ephemeral"}}],
			"tools":[{"name":"Read","description":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}}}}],
			"messages":[`+history+`]}`).(map[string]interface{})
		data, raw := anthropicToOpenAI(body)
		b, err := json.Marshal(openAICompatRequest(data, sanitizeMessages(raw)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "billing-header") {
			t.Fatalf("billing header reached the engine: %s", b)
		}
		var u upstream
		if err := json.Unmarshal(b, &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	first := `{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}`
	a := turn("aaaa", first)
	b := turn("bbbb", first+`,{"role":"assistant","content":"hello"},{"role":"user","content":"more"}`)

	// The engine renders tools and messages into one prompt; the tools and
	// every message of the earlier turn must come out byte-identical.
	if string(a.Tools) != string(b.Tools) {
		t.Fatalf("tools changed between turns:\n%s\n%s", a.Tools, b.Tools)
	}
	if len(a.Messages) != 2 || len(b.Messages) != 4 {
		t.Fatalf("messages: %d then %d, want 2 then 4", len(a.Messages), len(b.Messages))
	}
	for i := range a.Messages {
		if string(a.Messages[i]) != string(b.Messages[i]) {
			t.Fatalf("message %d changed between turns:\n%s\n%s", i, a.Messages[i], b.Messages[i])
		}
	}
	if string(a.Messages[0]) != `{"content":"You are Claude.","role":"system"}` {
		t.Fatalf("system message = %s", a.Messages[0])
	}
}

func TestCachedTokensBecomeCacheRead(t *testing.T) {
	payload := decodeJSON(t, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
		"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"prompt_tokens_details":{"cached_tokens":960}}}`)
	msg, err := openAIToAnthropic(payload, "m")
	if err != nil {
		t.Fatal(err)
	}
	u := msg.Usage
	if u.InputTokens != 40 || u.CacheReadInputTokens != 960 || u.CacheCreationInputTokens != 0 || u.OutputTokens != 5 {
		t.Fatalf("usage = %+v, want 40 uncached + 960 cache read", u)
	}
	if u.PromptTokens() != 1000 {
		t.Fatalf("PromptTokens = %d, want the full 1000 for logs and quota", u.PromptTokens())
	}
}

func TestCachedTokensClampedToPrompt(t *testing.T) {
	payload := decodeJSON(t, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":50}}}`)
	msg, _ := openAIToAnthropic(payload, "m")
	if msg.Usage.InputTokens != 0 || msg.Usage.CacheReadInputTokens != 10 {
		t.Fatalf("usage = %+v, want cached clamped to prompt", msg.Usage)
	}
}

func TestStreamUsageCarriesCacheRead(t *testing.T) {
	s := newAnthropicStream("m")
	s.start()
	s.chunk([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`))
	s.chunk([]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":500,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":480}}}`))
	var delta []byte
	for _, ev := range s.end() {
		if ev.name == "message_delta" {
			delta, _ = json.Marshal(ev.data)
		}
	}
	for _, want := range []string{`"input_tokens":20`, `"cache_read_input_tokens":480`,
		`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}`} {
		if !strings.Contains(string(delta), want) {
			t.Errorf("message_delta missing %s\n%s", want, delta)
		}
	}
}

// Gateway42 authors OpenAI usage too; an engine that reports no cache detail
// must not grow an empty prompt_tokens_details in those bodies.
func TestWireUsageOmitsAbsentDetails(t *testing.T) {
	payload := decodeJSON(t, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	msg, _ := openAIToAnthropic(payload, "m")
	if msg.Usage.CacheReadInputTokens != 0 || msg.Usage.InputTokens != 3 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
	b, _ := json.Marshal(wire.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4})
	if strings.Contains(string(b), "prompt_tokens_details") {
		t.Fatalf("authored usage gained prompt_tokens_details: %s", b)
	}
}
