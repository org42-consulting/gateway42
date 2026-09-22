package main

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────── ID generation ────────────────────────────────

// newCompletionID generates a unique completion ID for OpenAI responses
func newCompletionID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "chatcmpl-" + hex.EncodeToString(b)
}

// ─────────────────────────────── Request translation ──────────────────────────

// openAIToOllama translates an OpenAI chat completions request to Ollama /api/chat format.
// This function converts OpenAI API parameters to Ollama-compatible parameters
func openAIToOllama(data map[string]interface{}, messages []map[string]interface{}) map[string]interface{} {
	options := map[string]interface{}{}

	// Direct parameter mappings
	paramMap := map[string]string{
		"temperature":      "temperature",
		"top_p":            "top_p",
		"seed":             "seed",
		"max_tokens":       "num_predict",
		"presence_penalty": "repeat_last_n",
	}
	for openaiKey, ollamaKey := range paramMap {
		if v, ok := data[openaiKey]; ok {
			options[ollamaKey] = v
		}
	}

	// frequency_penalty → repeat_penalty = 1.0 + value
	if fp, ok := data["frequency_penalty"]; ok {
		switch v := fp.(type) {
		case float64:
			options["repeat_penalty"] = 1.0 + v
		case int:
			options["repeat_penalty"] = 1.0 + float64(v)
		}
	}

	// stop can be string or list → always list
	if stop, ok := data["stop"]; ok {
		switch v := stop.(type) {
		case string:
			options["stop"] = []string{v}
		case []interface{}:
			strs := make([]string, 0, len(v))
			for _, s := range v {
				if str, ok := s.(string); ok {
					strs = append(strs, str)
				}
			}
			options["stop"] = strs
		}
	}

	model := "llama3.2:latest"
	if m, ok := data["model"].(string); ok && m != "" {
		model = m
	}

	stream := false
	if s, ok := data["stream"].(bool); ok {
		stream = s
	}

	req := map[string]interface{}{
		"model":    model,
		"messages": messages,
		"stream":   stream,
	}
	if len(options) > 0 {
		req["options"] = options
	}
	return req
}

// ─────────────────────────────── Finish reason ────────────────────────────────

// ollamaFinishReason maps a *terminal* Ollama response onto OpenAI's
// finish_reason. Both response paths call it: the non-streaming envelope and
// the final streaming chunk, which are the only two places the value is
// non-null. It is never called mid-stream.
//
// TODO: decide what this returns. It currently answers "stop" unconditionally,
// which is what Gateway42 has always done — see OPENAI_COMPATIBILITY.md.
//
// Ollama reports done_reason on its final object: "stop" when the model emitted
// an end token, "length" when it hit num_predict. Older builds omit the field,
// and nothing stops a future one from reporting a reason OpenAI has no name for.
// OpenAI's vocabulary is "stop" | "length" | "content_filter" | "tool_calls".
//
// The decision is in the gaps, not the happy path:
//
//   - Defaulting an absent or unrecognised reason to "stop" keeps every client
//     working, but labels a truncated completion as naturally finished — so a
//     caller that retries on "length" silently never retries.
//   - Returning nil is honest about not knowing, but null is also what this
//     gateway sends mid-stream; on a terminal chunk some SDKs read it as "still
//     going" and a few fault outright.
//   - Forwarding an unrecognised value verbatim is most faithful to upstream,
//     but puts a string outside the enum in front of clients that switch on it
//     exhaustively.
//
// Two tests in types_test.go pin the current contract and will need updating if
// the terminal answer changes: TestChunkFinishReasonIsNullNotAbsent (present
// and null mid-stream) and TestCompletionUsageAlwaysPresent (asserts "stop" on
// a completed response).
//
// The field is a *string, so the value must be addressable: &finishStop
// (types.go) covers "stop"; for anything else a local works —
// r := "length"; return &r.
func ollamaFinishReason(chunk map[string]interface{}) *string {
	return &finishStop
}

// ─────────────────────────────── Non-streaming response ───────────────────────

// ollamaToOpenAI translates a complete Ollama /api/chat response to OpenAI format.
// The input stays a map because it is upstream-shaped; the output is typed
// because this is where Gateway42 authors the client-facing body.
func ollamaToOpenAI(ollama map[string]interface{}) *ChatCompletion {
	msg, _ := ollama["message"].(map[string]interface{})
	role, _ := msg["role"].(string)
	if role == "" {
		role = "assistant"
	}
	content, _ := msg["content"].(string)

	promptTokens := toInt(ollama["prompt_eval_count"])
	completionTokens := toInt(ollama["eval_count"])
	modelName, _ := ollama["model"].(string)

	return &ChatCompletion{
		ID:      newCompletionID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: role, Content: content},
			FinishReason: ollamaFinishReason(ollama),
		}},
		Usage: Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
}

// ─────────────────────────────── Streaming chunk translation ──────────────────

// formatStreamChunk translates one Ollama streaming line to an OpenAI SSE chunk.
//
// isFirst drives the role: OpenAI emits "role" only on the opening delta and
// clients accumulate from there, so repeating it would have some of them
// restart the message. The Delta struct's omitempty tags are what make the
// three shapes — opening, middle, terminating — fall out of one type.
func formatStreamChunk(chunk map[string]interface{}, completionID string, isFirst bool) *ChatCompletionChunk {
	done, _ := chunk["done"].(bool)
	msg, _ := chunk["message"].(map[string]interface{})
	content, _ := msg["content"].(string)

	var delta Delta
	switch {
	case isFirst:
		delta = Delta{Role: "assistant", Content: content}
	case done:
		// Terminating chunk: finish_reason carries the signal, not the delta.
		delta = Delta{}
	default:
		delta = Delta{Content: content}
	}

	// Only the terminal chunk carries a reason; mid-stream it stays null.
	var finishReason *string
	if done {
		finishReason = ollamaFinishReason(chunk)
	}

	modelName, _ := chunk["model"].(string)

	out := &ChatCompletionChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []ChunkChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		}},
	}

	// Ollama reports token counts only on its final object, and OpenAI puts
	// usage only on the last chunk, so the two line up exactly.
	if done {
		promptTokens := toInt(chunk["prompt_eval_count"])
		completionTokens := toInt(chunk["eval_count"])
		out.Usage = &Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
	}

	return out
}

// ─────────────────────────────── Models listing ───────────────────────────────

// parseOllamaTS converts Ollama timestamp string to Unix timestamp
func parseOllamaTS(ts string) int64 {
	if ts == "" {
		return time.Now().Unix()
	}
	if len(ts) >= 19 {
		ts = ts[:19]
	}
	t, err := time.Parse("2006-01-02T15:04:05", ts)
	if err != nil {
		return time.Now().Unix()
	}
	return t.Unix()
}

// ─────────────────────────────── Error helpers ────────────────────────────────

// openaiError creates an OpenAI-compatible error response
func openaiError(message, errorType string) *ErrorResponse {
	return &ErrorResponse{Error: APIError{Message: message, Type: errorType}}
}

// ─────────────────────────────── Helpers ──────────────────────────────────────

// toInt safely converts various numeric types to int
func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		// Atoi rather than Sscanf: Sscanf stops at the first non-digit and
		// reports success, so "12abc" parsed as 12 and "abc" left the result
		// at its zero value with the error dropped. Anything that is not a
		// whole number is not a count, and 0 is the same answer the rest of
		// this switch gives for an unusable value.
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0
		}
		return parsed
	}
	return 0
}
