package main

import (
	"gateway42/internal/wire"
	"strconv"
	"strings"
)

// OpenAI-side helpers shared by the handlers. The translation to and from
// Ollama's native API that used to live here went with Ollama support; every
// engine now speaks OpenAI's wire format, which is relayed as sent.

// ─────────────────────────────── Error helpers ────────────────────────────────

// openaiError creates an OpenAI-compatible error response
func openaiError(message, errorType string) *wire.ErrorResponse {
	return &wire.ErrorResponse{Error: wire.APIError{Message: message, Type: errorType}}
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
