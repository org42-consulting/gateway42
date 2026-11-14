// Package wire holds the OpenAI wire structures Gateway42 authors.
//
// It is a leaf package: it imports nothing from the rest of the gateway, so
// the adapters, translators and handlers can all depend on the same
// contract without depending on each other.
package wire

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
	// ReasoningContent is the model's reasoning, kept apart from the answer:
	// the de-facto field (DeepSeek, oMLX, older vLLM) for what Ollama calls
	// "thinking". Reasoning is newer vLLM's name for the same thing; it is
	// only ever read, never authored. Both omitempty, so a reply without
	// reasoning is the same body it always was.
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Reasoning        string     `json:"reasoning,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// ReasoningText is the message's reasoning under whichever name the engine
// used for it.
func (m Message) ReasoningText() string {
	if m.ReasoningContent != "" {
		return m.ReasoningContent
	}
	return m.Reasoning
}

// Delta is the incremental payload in a streaming chunk. Every field is
// optional: the first chunk carries the role, middle chunks carry content or
// tool calls, and the terminating chunk carries none — which is why this
// cannot reuse Message, whose role and content are always emitted.
type Delta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"` // see Message
	ToolCalls        []StreamToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is a function call the model asked the client to make.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function FunctionCall `json:"function"`
}

// StreamToolCall is a ToolCall inside a streaming delta. OpenAI clients
// assemble calls across chunks by index, so it is required on the wire —
// including index 0, which is why it is not omitempty.
type StreamToolCall struct {
	Index int `json:"index"`
	ToolCall
}

// FunctionCall names the function and its arguments. Arguments is a string
// of JSON, not an object: that is the OpenAI schema, and SDKs json.loads it.
// Ollama sends an object, so the translation re-encodes it.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage reports token accounting. Values are always emitted, including zero:
// clients distinguish "no tokens" from "not reported" by the object's presence,
// so omitempty on the fields would make a cheap completion look unmeasured.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// PromptTokensDetails carries the engine's prefix-cache hits (oMLX, vLLM,
	// llama.cpp and OpenAI report cached_tokens here). A pointer with
	// omitempty: Gateway42 never authors it, so bodies it writes are unchanged,
	// and absence means "engine did not say", not "nothing was cached".
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetails is the breakdown of prompt_tokens an engine may report.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// CachedTokens is the number of prompt tokens the engine served from its
// prefix cache, clamped to PromptTokens; 0 when the engine did not report it.
func (u Usage) CachedTokens() int {
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens < 0 {
		return 0
	}
	return min(u.PromptTokensDetails.CachedTokens, u.PromptTokens)
}

// Choice is one completion alternative in a non-streaming response.
//
// FinishReason is a pointer because the OpenAI schema distinguishes null (the
// completion is still going) from a string. Encoding it as "" would tell a
// client the stream ended for an unknown reason.
type Choice struct {
	Index        int       `json:"index"`
	Message      Message   `json:"message"`
	Logprobs     *Logprobs `json:"logprobs,omitempty"`
	FinishReason *string   `json:"finish_reason"`
}

// Logprobs is a choice's per-token log probabilities. Content holds OpenAI's
// token objects ({token, logprob, bytes, top_logprobs}), which is also the
// shape Ollama reports them in, so they are carried as decoded.
type Logprobs struct {
	Content []interface{} `json:"content"`
}

// ChunkChoice is one completion alternative in a streaming chunk.
type ChunkChoice struct {
	Index        int       `json:"index"`
	Delta        Delta     `json:"delta"`
	Logprobs     *Logprobs `json:"logprobs,omitempty"`
	FinishReason *string   `json:"finish_reason"`
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

// FinishStop, FinishLength and FinishToolCalls are the address-of-able values
// FinishReason points at. Sharing them is safe because nothing ever writes
// through these pointers.
var (
	FinishStop      = "stop"
	FinishLength    = "length"
	FinishToolCalls = "tool_calls"
)

// ── Models ────────────────────────────────────────────────────────────────────

// Model is one entry in a /v1/models listing.
//
// One listing serves both OpenAI and Anthropic clients, so each entry carries
// both schemas' fields: OpenAI's object/created/owned_by and Anthropic's
// type/display_name/created_at. Each SDK ignores the other's keys, which is
// simpler and more robust than choosing a shape by sniffing request headers.
//
// Size is a Gateway42 extension, not part of either schema — omitted when
// zero so engines that do not report it (anything OpenAI-compatible) produce a
// response a strict client still accepts.
type Model struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
	Type        string `json:"type"`         // Anthropic: always "model"
	DisplayName string `json:"display_name"` // Anthropic
	CreatedAt   string `json:"created_at"`   // Anthropic: RFC 3339
	Size        int64  `json:"size,omitempty"`
	// MaxInputTokens is the model's context window as its engine reports it
	// (oMLX and vLLM: max_model_len; LM Studio: max_context_length; others:
	// context_length / context_window). Claude Desktop reads it from
	// discovery; omitted when the engine does not say.
	MaxInputTokens int `json:"max_input_tokens,omitempty"`
	// AnthropicFamilyTier and IsFamilyDefault are relayed when an engine
	// labels a model with the Claude tier it stands in for (oMLX does for its
	// Claude Desktop aliases), so the app can pick it for a sub-agent that
	// asks for "sonnet" or "haiku".
	AnthropicFamilyTier string `json:"anthropic_family_tier,omitempty"`
	IsFamilyDefault     bool   `json:"is_family_default,omitempty"`
}

// ModelList is the /v1/models response body. HasMore, FirstID and LastID are
// Anthropic's pagination fields; the gateway returns everything in one page.
type ModelList struct {
	Object  string  `json:"object"`
	Data    []Model `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
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
