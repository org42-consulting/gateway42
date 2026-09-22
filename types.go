package main

// Typed OpenAI wire structs.
//
// Scope is deliberate: this covers everything Gateway42 *writes* — completions,
// chunks, the model list and errors — and nothing it merely forwards. Responses
// are authored here, so a typo in a field name should be a compile error rather
// than a key that silently never reaches the client, and `omitempty` should be
// stated once per field rather than re-derived at every construction site.
//
// Inbound requests stay as map[string]interface{} on purpose. Unrecognised
// parameters are passed through verbatim to OpenAI-compatible engines
// (handleChatCompletions), so a struct would have to model the entire, moving
// OpenAI parameter surface just to avoid dropping fields on the floor. A map is
// the honest representation of "forward what we were given".

// ── Chat completions ──────────────────────────────────────────────────────────

// Message is one turn in a completed (non-streaming) response.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Delta is the incremental payload in a streaming chunk. Every field is
// optional: the first chunk carries the role, middle chunks carry content, and
// the terminating chunk carries neither — which is why this cannot reuse
// Message, whose fields are always emitted.
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// Usage reports token accounting. Values are always emitted, including zero:
// clients distinguish "no tokens" from "not reported" by the object's presence,
// so omitempty on the fields would make a cheap completion look unmeasured.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice is one completion alternative in a non-streaming response.
//
// FinishReason is a pointer because the OpenAI schema distinguishes null (the
// completion is still going) from a string. Encoding it as "" would tell a
// client the stream ended for an unknown reason.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

// ChunkChoice is one completion alternative in a streaming chunk.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// ChatCompletion is a non-streaming response body.
type ChatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// ChatCompletionChunk is one SSE frame's payload.
//
// Usage is a pointer and omitted on all but the final chunk, matching OpenAI:
// a zeroed usage object on every chunk would have clients that sum them
// over-count by the number of tokens times the number of chunks.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// finishStop is the address-of-able "stop" that FinishReason points at. One
// shared value is safe because nothing ever writes through these pointers.
var finishStop = "stop"

// ── Models ────────────────────────────────────────────────────────────────────

// Model is one entry in a /v1/models listing.
//
// Size is a Gateway42 extension, not part of the OpenAI schema — omitted when
// zero so engines that do not report it (anything OpenAI-compatible) produce a
// response a strict client still accepts.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	Size    int64  `json:"size,omitempty"`
}

// ModelList is the /v1/models response body.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ── Errors ────────────────────────────────────────────────────────────────────

// APIError is the inner object of an OpenAI-shaped error response.
//
// Code is an explicit pointer so it marshals as null rather than being dropped.
// Clients that read err.code and expect the key to exist — the OpenAI SDKs
// among them — break on its absence, not on its being null.
type APIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    *string `json:"code"`
}

// ErrorResponse wraps APIError in the envelope every /v1 error uses.
type ErrorResponse struct {
	Error APIError `json:"error"`
}
