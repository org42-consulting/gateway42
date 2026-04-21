package main

import (
	"encoding/hex"
	"crypto/rand"
	"fmt"
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
		"temperature":     "temperature",
		"top_p":           "top_p",
		"seed":            "seed",
		"max_tokens":      "num_predict",
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

// ─────────────────────────────── Non-streaming response ───────────────────────

// ollamaToOpenAI translates a complete Ollama /api/chat response to OpenAI format.
// This function converts Ollama response format to OpenAI-compatible format
func ollamaToOpenAI(ollama map[string]interface{}) map[string]interface{} {
	msg, _ := ollama["message"].(map[string]interface{})
	if msg == nil {
		msg = map[string]interface{}{}
	}
	role, _ := msg["role"].(string)
	if role == "" {
		role = "assistant"
	}
	content, _ := msg["content"].(string)

	promptTokens := toInt(ollama["prompt_eval_count"])
	completionTokens := toInt(ollama["eval_count"])
	modelName, _ := ollama["model"].(string)

	return map[string]interface{}{
		"id":      newCompletionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role":    role,
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
}

// ─────────────────────────────── Streaming chunk translation ──────────────────

// formatStreamChunk translates one Ollama streaming line to an OpenAI SSE chunk.
// This function converts streaming response chunks from Ollama to OpenAI-compatible format
func formatStreamChunk(chunk map[string]interface{}, completionID string, isFirst bool) map[string]interface{} {
	done, _ := chunk["done"].(bool)
	msg, _ := chunk["message"].(map[string]interface{})
	content := ""
	if msg != nil {
		content, _ = msg["content"].(string)
	}

	var delta map[string]interface{}
	if isFirst {
		delta = map[string]interface{}{"role": "assistant", "content": content}
	} else if done {
		delta = map[string]interface{}{}
	} else {
		delta = map[string]interface{}{"content": content}
	}

	finishReason := interface{}(nil)
	if done {
		finishReason = "stop"
	}

	modelName, _ := chunk["model"].(string)

	result := map[string]interface{}{
		"id":      completionID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}

	if done {
		promptTokens := toInt(chunk["prompt_eval_count"])
		completionTokens := toInt(chunk["eval_count"])
		result["usage"] = map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		}
	}

	return result
}

// ─────────────────────────────── Models listing ───────────────────────────────

// ollamaTagsToOpenAIModels translates Ollama GET /api/tags to OpenAI /v1/models format.
// This function converts Ollama's model listing format to OpenAI-compatible format
func ollamaTagsToOpenAIModels(tags map[string]interface{}) map[string]interface{} {
	models, _ := tags["models"].([]interface{})
	data := make([]interface{}, 0, len(models))
	for _, m := range models {
		model, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := model["name"].(string)
		ts, _ := model["modified_at"].(string)
		created := parseOllamaTS(ts)
		data = append(data, map[string]interface{}{
			"id":       name,
			"object":   "model",
			"created":  created,
			"owned_by": "ollama",
		})
	}
	return map[string]interface{}{"object": "list", "data": data}
}

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
func openaiError(message, errorType string) map[string]interface{} {
	return map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errorType,
			"code":    nil,
		},
	}
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
		var result int
		fmt.Sscanf(n, "%d", &result)
		return result
	}
	return 0
}
