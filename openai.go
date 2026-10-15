package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
		"messages": ollamaMessages(messages),
		"stream":   stream,
	}
	if len(options) > 0 {
		req["options"] = options
	}

	// Ollama takes OpenAI's tool schema as-is. It has no tool_choice, so the
	// one value that can be honoured is "none": withhold the tools entirely.
	if tools, ok := data["tools"].([]interface{}); ok && len(tools) > 0 {
		if choice, _ := data["tool_choice"].(string); choice != "none" {
			req["tools"] = tools
		}
	}
	if format := ollamaFormat(data["response_format"]); format != nil {
		req["format"] = format
	}
	return req
}

// ollamaFormat maps OpenAI's response_format onto Ollama's format field:
// "json" for JSON mode, or the schema object itself for structured output.
// Anything else — {"type":"text"}, or a shape not recognised — returns nil,
// which leaves format unset and the reply unconstrained.
func ollamaFormat(rf interface{}) interface{} {
	m, ok := rf.(map[string]interface{})
	if !ok {
		return nil
	}
	switch m["type"] {
	case "json_object":
		return "json"
	case "json_schema":
		js, _ := m["json_schema"].(map[string]interface{})
		if schema, ok := js["schema"].(map[string]interface{}); ok {
			return schema
		}
		// A json_schema request without a usable schema still asked for
		// JSON; plain JSON mode is the closest Ollama can get.
		return "json"
	}
	return nil
}

// ollamaMessages converts an OpenAI message history into Ollama's shape. The
// differences that matter are all in tool use and multimodal content:
//
//   - Assistant tool_calls carry arguments as a JSON *string* in OpenAI and as
//     an object in Ollama.
//   - A tool result is linked to its call by tool_call_id in OpenAI; Ollama
//     has no call IDs and wants the function's name as tool_name instead,
//     which is recovered from the assistant turn that issued the call.
//   - Content may be an array of parts. Ollama wants one content string plus
//     a separate images list of bare base64. Only inline data: URLs can be
//     passed on; a remote image URL would need fetching, which the gateway
//     does not do, so it is dropped.
func ollamaMessages(messages []map[string]interface{}) []map[string]interface{} {
	nameByCallID := map[string]string{}
	out := make([]map[string]interface{}, 0, len(messages))

	for _, m := range messages {
		om := map[string]interface{}{"role": m["role"]}

		text, images := flattenContent(m["content"])
		om["content"] = text
		if len(images) > 0 {
			om["images"] = images
		}

		if calls, ok := m["tool_calls"].([]interface{}); ok && len(calls) > 0 {
			ollamaCalls := make([]map[string]interface{}, 0, len(calls))
			for _, c := range calls {
				call, _ := c.(map[string]interface{})
				fn, _ := call["function"].(map[string]interface{})
				name, _ := fn["name"].(string)
				if id, _ := call["id"].(string); id != "" {
					nameByCallID[id] = name
				}
				ollamaCalls = append(ollamaCalls, map[string]interface{}{
					"function": map[string]interface{}{
						"name":      name,
						"arguments": decodeArguments(fn["arguments"]),
					},
				})
			}
			om["tool_calls"] = ollamaCalls
		}

		if m["role"] == "tool" {
			name, _ := m["name"].(string)
			if name == "" {
				id, _ := m["tool_call_id"].(string)
				name = nameByCallID[id]
			}
			if name != "" {
				om["tool_name"] = name
			}
		}
		out = append(out, om)
	}
	return out
}

// flattenContent splits OpenAI message content into Ollama's text and images.
func flattenContent(content interface{}) (string, []string) {
	switch c := content.(type) {
	case string:
		return c, nil
	case []interface{}:
		var text strings.Builder
		var images []string
		for _, p := range c {
			part, _ := p.(map[string]interface{})
			switch part["type"] {
			case "text":
				s, _ := part["text"].(string)
				if text.Len() > 0 && s != "" {
					text.WriteString("\n")
				}
				text.WriteString(s)
			case "image_url":
				iu, _ := part["image_url"].(map[string]interface{})
				u, _ := iu["url"].(string)
				if _, b64, ok := strings.Cut(u, ";base64,"); ok && strings.HasPrefix(u, "data:") {
					images = append(images, b64)
				}
			}
		}
		return text.String(), images
	}
	return "", nil
}

// decodeArguments turns OpenAI's string-encoded arguments into the object
// Ollama expects. Arguments that are already an object pass through; a
// string that is not valid JSON is passed as-is so the engine reports it
// rather than the gateway silently replacing it.
func decodeArguments(args interface{}) interface{} {
	s, ok := args.(string)
	if !ok {
		return args
	}
	var obj interface{}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return s
	}
	return obj
}

// openAIToolCalls converts the tool calls on an Ollama message into OpenAI's
// shape, or returns nil when there are none.
//
// Ollama does not assign call IDs (some builds do; those are kept), but an
// OpenAI client must echo one back as tool_call_id on the result, so one is
// minted per call.
func openAIToolCalls(msg map[string]interface{}) []ToolCall {
	raw, _ := msg["tool_calls"].([]interface{})
	if len(raw) == 0 {
		return nil
	}
	calls := make([]ToolCall, 0, len(raw))
	for _, c := range raw {
		call, _ := c.(map[string]interface{})
		fn, _ := call["function"].(map[string]interface{})
		name, _ := fn["name"].(string)

		args, isString := fn["arguments"].(string)
		if !isString {
			args = "{}"
			if fn["arguments"] != nil {
				if b, err := json.Marshal(fn["arguments"]); err == nil {
					args = string(b)
				}
			}
		}

		id, _ := call["id"].(string)
		if id == "" {
			id = newToolCallID()
		}
		calls = append(calls, ToolCall{
			ID:       id,
			Type:     "function",
			Function: FunctionCall{Name: name, Arguments: args},
		})
	}
	return calls
}

// newToolCallID mints an OpenAI-style call ID.
func newToolCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}

// ─────────────────────────────── Finish reason ────────────────────────────────

// ollamaFinishReason maps a *terminal* Ollama response onto OpenAI's
// finish_reason. Both response paths call it: the non-streaming envelope and
// the final streaming chunk, which are the only two places the value is
// non-null. It is never called mid-stream.
//
// Ollama reports done_reason on its final object: "stop" when the model emitted
// an end token, "length" when it hit num_predict (which max_tokens maps to).
// OpenAI's vocabulary is "stop" | "length" | "content_filter" | "tool_calls".
//
// "length" is the value that matters: it is how a client learns its completion
// was truncated, and callers continue or retry on it. Labelling a truncated
// reply "stop" makes a half answer look finished.
//
// Everything else — an absent field (older Ollama builds), or a reason OpenAI
// has no name for — maps to "stop". The alternatives were rejected:
//
//   - nil is what this gateway sends mid-stream; on a terminal chunk some SDKs
//     read it as "still going" and a few fault outright.
//   - Forwarding an unrecognised value verbatim puts a string outside the enum
//     in front of clients that switch on it exhaustively.
//
// "stop" is what Gateway42 answered unconditionally before, so the fallback
// is never worse than the old behaviour.
func ollamaFinishReason(chunk map[string]interface{}) *string {
	if reason, _ := chunk["done_reason"].(string); reason == finishLength {
		return &finishLength
	}
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

	// A reply that calls tools finished *because* it called them, which is
	// what tool_calls means; Ollama reports that case as an ordinary "stop".
	toolCalls := openAIToolCalls(msg)
	finishReason := ollamaFinishReason(ollama)
	if len(toolCalls) > 0 {
		finishReason = &finishToolCalls
	}

	return &ChatCompletion{
		ID:      newCompletionID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: role, Content: content, ToolCalls: toolCalls},
			FinishReason: finishReason,
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

	// Ollama emits each tool call whole, in one line, so each becomes one
	// complete StreamToolCall. Index is numbered within this chunk only: a
	// stream's calls are numbered across chunks, which only the stream itself
	// can do — ollamaStream renumbers them.
	for i, tc := range openAIToolCalls(msg) {
		delta.ToolCalls = append(delta.ToolCalls, StreamToolCall{Index: i, ToolCall: tc})
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
