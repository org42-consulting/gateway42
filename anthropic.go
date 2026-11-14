package main

// Anthropic Messages API front-end.
//
// Claude Code (and every Anthropic SDK) speaks POST /v1/messages, not OpenAI's
// chat/completions. Rather than teach each engine adapter a second protocol,
// this file translates at the edge: an Anthropic request becomes the same
// OpenAI-shaped request the rest of the gateway already routes, and the
// OpenAI-shaped result every adapter produces is translated back. Routing,
// rate limiting, concurrency slots, metrics and the audit log are shared with
// /v1/chat/completions unchanged.
//
// As with the OpenAI side, inbound bodies stay maps and outbound bodies are
// typed: Gateway42 authors what it writes and forwards what it is given.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"gateway42/internal/wire"
	"strings"
)

// ── Wire types ────────────────────────────────────────────────────────────────

// AnthropicContentBlock is one block of an assistant message. Text blocks use
// Text; tool_use blocks use ID, Name and Input. Text has no omitempty because
// an empty text block must still carry "text":"" — content_block_start opens
// every text block that way, and SDKs read the key unconditionally.
//
// Thinking blocks use Thinking and Signature, pointers for the same reason:
// content_block_start opens one as {"thinking":"","signature":""}. The
// signature stays empty — it authenticates reasoning Anthropic generated,
// and a local engine's reasoning has nothing to sign. Clients send it back
// with the block, and the request translation drops both again.
type AnthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature *string         `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// thinkingBlock is a thinking content block holding text.
func thinkingBlock(text string) AnthropicContentBlock {
	empty := ""
	return AnthropicContentBlock{Type: "thinking", Thinking: &text, Signature: &empty}
}

// thinkingRequested reports whether a /v1/messages request asked for the
// model's reasoning ("thinking": {"type": "enabled" | "adaptive"}). Only then
// is an engine's reasoning returned as thinking blocks: a client that did not
// ask may not expect them, and dropping reasoning is what the gateway always
// did for such clients.
func thinkingRequested(body map[string]interface{}) bool {
	t, _ := body["thinking"].(map[string]interface{})
	switch t["type"] {
	case "enabled", "adaptive":
		return true
	}
	return false
}

// anthropicEffort maps the request's output_config.effort — the Effort control
// in Claude Desktop and Claude Code — onto OpenAI's reasoning_effort, which
// oMLX, vLLM and LM Studio hand to the chat template. "max" has no common
// OpenAI equivalent above "high", the top value every template accepts.
// Returns "" when the request sets no effort.
func anthropicEffort(body map[string]interface{}) string {
	oc, _ := body["output_config"].(map[string]interface{})
	switch e, _ := oc["effort"].(string); e {
	case "low", "medium", "high":
		return e
	case "max", "xhigh":
		return "high"
	}
	return ""
}

// withoutThinking removes thinking blocks from a translated message.
func withoutThinking(msg *AnthropicMessage) {
	kept := msg.Content[:0]
	for _, b := range msg.Content {
		if b.Type != "thinking" {
			kept = append(kept, b)
		}
	}
	msg.Content = kept
}

// AnthropicUsage reports token accounting. Every field is always emitted,
// matching Usage on the OpenAI side.
//
// As on Anthropic's API, the prompt is split three ways: InputTokens is what
// the engine had to process, CacheReadInputTokens what it served from its
// prefix cache, and CacheCreationInputTokens what it wrote to a cache. The
// client adds all three to size its context window, so the split never
// changes that total. Local engines cache every prompt implicitly and report
// no writes, so creation is always 0 — but the cache_creation object is still
// sent, because Claude Desktop reads its two TTL fields from it.
type AnthropicUsage struct {
	InputTokens              int                    `json:"input_tokens"`
	CacheCreationInputTokens int                    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                    `json:"cache_read_input_tokens"`
	CacheCreation            AnthropicCacheCreation `json:"cache_creation"`
	OutputTokens             int                    `json:"output_tokens"`
}

// AnthropicCacheCreation splits cache writes by requested lifetime.
type AnthropicCacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

// PromptTokens is the whole prompt as the engine counted it — the figure the
// request log and the quota record, whatever share of it came from cache.
func (u AnthropicUsage) PromptTokens() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// anthropicUsage converts OpenAI-shaped usage, moving the engine's reported
// prefix-cache hits from input_tokens to cache_read_input_tokens.
func anthropicUsage(u wire.Usage) AnthropicUsage {
	cached := u.CachedTokens()
	return AnthropicUsage{
		InputTokens:          u.PromptTokens - cached,
		CacheReadInputTokens: cached,
		OutputTokens:         u.CompletionTokens,
	}
}

// AnthropicMessage is a non-streaming /v1/messages response, and also the
// "message" inside a streaming message_start event (with empty content).
//
// StopReason and StopSequence are pointers: the schema has them null while a
// stream is in progress, and Claude Code distinguishes null from a value.
type AnthropicMessage struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"` // always "message"
	Role         string                  `json:"role"`
	Model        string                  `json:"model"`
	Content      []AnthropicContentBlock `json:"content"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        AnthropicUsage          `json:"usage"`
}

// AnthropicErrorResponse is the error envelope for every Anthropic-path error.
type AnthropicErrorResponse struct {
	Type  string            `json:"type"` // always "error"
	Error AnthropicAPIError `json:"error"`
}

// AnthropicAPIError is the inner object of an Anthropic error.
type AnthropicAPIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

var (
	stopEndTurn   = "end_turn"
	stopMaxTokens = "max_tokens"
	stopToolUse   = "tool_use"
)

// anthropicError builds an Anthropic-shaped error. The type names overlap
// OpenAI's for the cases the gateway produces, with two exceptions mapped by
// status: Anthropic reports a missing model as not_found_error and
// backpressure as overloaded_error, and Claude Code's retry logic keys on
// those names.
func anthropicError(status int, message, errType string) *AnthropicErrorResponse {
	switch status {
	case 404:
		errType = "not_found_error"
	case 413:
		errType = "request_too_large"
	case 429:
		errType = "rate_limit_error"
	case 503, 529:
		errType = "overloaded_error"
	}
	return &AnthropicErrorResponse{Type: "error", Error: AnthropicAPIError{Type: errType, Message: message}}
}

func newMessageID() string { return "msg_" + randomHex(12) }
func newToolUseID() string { return "toolu_" + randomHex(12) }

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ── Request translation ──────────────────────────────────────────────────────

// anthropicToOpenAI converts a /v1/messages body to the OpenAI request map the
// chat-completions pipeline consumes. The returned messages are raw (not yet
// sanitised) so the caller runs them through the same sanitizeMessages as the
// OpenAI path.
//
// Parameters with no OpenAI equivalent — metadata, container, mcp_servers —
// are dropped: an engine that cannot honour them would only reject the
// request. top_k is forwarded, since local engines take it (Ollama as an
// option).
//
// cache_control is dropped too, but caching is not: oMLX, vLLM, llama.cpp,
// LM Studio and Ollama all reuse the KV cache for the longest prompt prefix
// they have seen, with no per-request marker. What they need is a prefix that
// is byte-identical from one turn to the next, so this translation must be
// deterministic (json.Marshal sorts map keys, and nothing here depends on the
// time or on randomness), and systemPrompt removes the one line Claude
// clients put at the top of the prompt that changes on every request.
func anthropicToOpenAI(data map[string]interface{}) (map[string]interface{}, []interface{}) {
	out := map[string]interface{}{"model": data["model"]}
	for _, key := range []string{"max_tokens", "temperature", "top_p", "top_k", "stream"} {
		if v, ok := data[key]; ok {
			out[key] = v
		}
	}
	if stops, ok := data["stop_sequences"].([]interface{}); ok && len(stops) > 0 {
		out["stop"] = stops
	}
	if tools := anthropicTools(data["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if tc := anthropicToolChoice(data["tool_choice"]); tc != nil {
			out["tool_choice"] = tc
		}
		if tc, _ := data["tool_choice"].(map[string]interface{}); tc["disable_parallel_tool_use"] == true {
			out["parallel_tool_calls"] = false
		}
	}

	// Thinking is switched on the engine, not only filtered on the way back.
	// Anthropic's default is no extended thinking, so a request without
	// "thinking" (or with type "disabled") turns it off; "enabled" and
	// "adaptive" turn it on. The switch local engines read is the chat
	// template's enable_thinking (Qwen3, DeepSeek, GLM and others; a template
	// without the variable ignores it), sent as chat_template_kwargs, which
	// openAIToOllama turns into Ollama's think:false. Without it a reasoning
	// model thinks on every turn the client asked it not to — minutes of
	// tokens on a local model, all thrown away by withoutThinking.
	out["chat_template_kwargs"] = map[string]interface{}{"enable_thinking": thinkingRequested(data)}
	// budget_tokens caps the thinking. oMLX reads it as thinking_budget; an
	// engine without a budget ignores the field.
	if t, _ := data["thinking"].(map[string]interface{}); thinkingRequested(data) && toInt(t["budget_tokens"]) > 0 {
		out["thinking_budget"] = toInt(t["budget_tokens"])
	}
	if effort := anthropicEffort(data); effort != "" {
		out["reasoning_effort"] = effort
	}

	var messages []interface{}
	if sys := systemPrompt(data["system"]); sys != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": sys})
	}
	raw, _ := data["messages"].([]interface{})
	for _, m := range raw {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		messages = append(messages, anthropicMessageToOpenAI(msg)...)
	}
	return out, messages
}

// anthropicMessageToOpenAI expands one Anthropic turn into OpenAI messages.
// One turn can become several: tool results are their own "tool" messages in
// OpenAI, and they must come before any text the user sent alongside them,
// because OpenAI requires tool results to follow the assistant's calls
// directly.
func anthropicMessageToOpenAI(msg map[string]interface{}) []interface{} {
	role, _ := msg["role"].(string)
	if s, ok := msg["content"].(string); ok {
		return []interface{}{map[string]interface{}{"role": role, "content": s}}
	}
	blocks, _ := msg["content"].([]interface{})

	var out []interface{}
	var parts []interface{}
	var toolCalls []interface{}
	hasImage := false

	for _, b := range blocks {
		block, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			text, _ := block["text"].(string)
			parts = append(parts, map[string]interface{}{"type": "text", "text": text})
		case "image":
			if url := imageDataURL(block["source"]); url != "" {
				hasImage = true
				parts = append(parts, map[string]interface{}{
					"type": "image_url", "image_url": map[string]interface{}{"url": url},
				})
			}
		case "tool_use":
			args, err := json.Marshal(block["input"])
			if err != nil || block["input"] == nil {
				args = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   block["id"],
				"type": "function",
				"function": map[string]interface{}{
					"name":      block["name"],
					"arguments": string(args),
				},
			})
		case "document":
			if fp := documentFilePart(block); fp != nil {
				hasImage = true // array content, as for images
				parts = append(parts, fp)
			} else if text := documentText(block); text != "" {
				parts = append(parts, map[string]interface{}{"type": "text", "text": text})
			}
		case "tool_result":
			content := blocksText(block["content"])
			// OpenAI tool messages carry text only, so a tool's images — the
			// screenshots computer use and browser tools return — travel in
			// the user turn that follows the tool messages, labelled with the
			// call they belong to. Dropping them, as before, left the model
			// blind to what the tool showed.
			media := mediaParts(block["content"])
			if len(media) > 0 {
				hasImage = true
				parts = append(parts, map[string]interface{}{
					"type": "text", "text": fmt.Sprintf("[Output of tool call %v]", block["tool_use_id"]),
				})
				parts = append(parts, media...)
				if content == "" {
					content = "[The tool returned images or files; they follow in the next message.]"
				}
			}
			if isErr, _ := block["is_error"].(bool); isErr {
				content = "Error: " + content
			}
			out = append(out, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": block["tool_use_id"],
				"content":      content,
			})
		}
		// thinking / redacted_thinking blocks are the model's own scratch
		// work from a previous turn; no OpenAI engine accepts them back.
	}

	if len(parts) == 0 && len(toolCalls) == 0 {
		return out
	}
	turn := map[string]interface{}{"role": role}
	switch {
	case hasImage:
		turn["content"] = parts
	case len(parts) > 0:
		// Text-only content collapses to a string: every engine accepts that,
		// while array content is not universal on local servers.
		turn["content"] = joinTextParts(parts)
	default:
		turn["content"] = nil
	}
	if len(toolCalls) > 0 {
		turn["tool_calls"] = toolCalls
	}
	return append(out, turn)
}

// billingHeaderPrefix opens the attribution line Claude Code, Claude Desktop
// and Cowork prepend to the system prompt, e.g.
//
//	x-anthropic-billing-header: cc_version=2.1.37.a1b; cc_entrypoint=cli; cch=9f3c2;
//
// Anthropic's API reads it as metadata. To a local engine it is just the
// first tokens of the prompt, and its cch hash differs on every request, so
// every turn misses the prefix cache at token zero and the engine reprocesses
// the whole conversation — tools, system prompt and history.
const billingHeaderPrefix = "x-anthropic-billing-header:"

// systemPrompt flattens the system prompt like blocksText, minus the billing
// header: dropped as a block when sent as its own text block (how the clients
// send it), or as the first line when the system prompt is a single string.
func systemPrompt(v interface{}) string {
	switch c := v.(type) {
	case string:
		if strings.HasPrefix(c, billingHeaderPrefix) {
			_, rest, _ := strings.Cut(c, "\n")
			return strings.TrimLeft(rest, "\n")
		}
		return c
	case []interface{}:
		kept := make([]interface{}, 0, len(c))
		for _, b := range c {
			if block, ok := b.(map[string]interface{}); ok {
				if t, _ := block["text"].(string); strings.HasPrefix(t, billingHeaderPrefix) {
					continue
				}
			}
			kept = append(kept, b)
		}
		return blocksText(kept)
	}
	return ""
}

// blocksText flattens a string-or-blocks value (system prompt, tool_result
// content) to plain text, joining text blocks with blank lines.
func blocksText(v interface{}) string {
	switch c := v.(type) {
	case string:
		return c
	case []interface{}:
		var texts []string
		for _, b := range c {
			block, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			var t string
			switch block["type"] {
			case "text":
				t, _ = block["text"].(string)
			case "document":
				// Tools may return documents too (search and file tools).
				t = documentText(block)
			}
			if t != "" {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, "\n\n")
	}
	return ""
}

// mediaParts collects the image and PDF blocks of a block list (a
// tool_result's content) as OpenAI image_url and file parts. A string or
// text-only list has none.
func mediaParts(v interface{}) []interface{} {
	blocks, _ := v.([]interface{})
	var parts []interface{}
	for _, b := range blocks {
		block, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		switch block["type"] {
		case "image":
			if url := imageDataURL(block["source"]); url != "" {
				parts = append(parts, map[string]interface{}{
					"type": "image_url", "image_url": map[string]interface{}{"url": url},
				})
			}
		case "document":
			if fp := documentFilePart(block); fp != nil {
				parts = append(parts, fp)
			}
		}
	}
	return parts
}

// documentFilePart turns a base64 PDF document into OpenAI's file content
// part ({"type":"file","file":{"filename","file_data"}}), which oMLX converts
// to text itself (its MarkItDown integration) and the OpenAI platform reads
// natively. Engines that take no files get a note instead — Ollama via
// flattenContent. nil for any other document.
func documentFilePart(block map[string]interface{}) map[string]interface{} {
	src, _ := block["source"].(map[string]interface{})
	mt, _ := src["media_type"].(string)
	data, _ := src["data"].(string)
	if src["type"] != "base64" || mt != "application/pdf" || data == "" {
		return nil
	}
	name, _ := block["title"].(string)
	if name == "" {
		name = "document"
	}
	// The engine picks a converter by extension.
	if !strings.HasSuffix(strings.ToLower(name), ".pdf") {
		name += ".pdf"
	}
	return map[string]interface{}{"type": "file", "file": map[string]interface{}{
		"filename": name, "file_data": "data:application/pdf;base64," + data,
	}}
}

// documentText renders an Anthropic document block as text the model reads
// in place of the document, headed by its title and context when given.
//
// Plain-text and content-block documents are inlined whole. A PDF (base64,
// URL or Files API reference) cannot be: no local engine reads PDFs, and the
// gateway has no PDF parser. It becomes a one-line note instead of vanishing,
// so the model knows a document was attached and can say it cannot read it
// rather than answer as if nothing had been sent.
func documentText(block map[string]interface{}) string {
	src, _ := block["source"].(map[string]interface{})
	title, _ := block["title"].(string)
	var body string
	switch src["type"] {
	case "text":
		body, _ = src["data"].(string)
	case "content":
		body = blocksText(src["content"])
	case "base64":
		if documentFilePart(block) != nil {
			return "" // sent as a file part, see documentFilePart
		}
		fallthrough
	default:
		name := title
		if name == "" {
			name = "untitled"
		}
		where := ""
		if u, _ := src["url"].(string); u != "" {
			where = " at " + u
		}
		mt, _ := src["media_type"].(string)
		if mt == "" {
			mt = "document"
		}
		return fmt.Sprintf("[Attached %s %q%s could not be included: the gateway can pass on PDFs only as inline (base64) data.]", mt, name, where)
	}
	if body == "" {
		return ""
	}
	var head []string
	if title != "" {
		head = append(head, "Document: "+title)
	}
	if ctx, _ := block["context"].(string); ctx != "" {
		head = append(head, ctx)
	}
	if len(head) == 0 {
		return body
	}
	return strings.Join(head, "\n") + "\n\n" + body
}

func joinTextParts(parts []interface{}) string {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if t, _ := p.(map[string]interface{})["text"].(string); t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n\n")
}

// imageDataURL turns an Anthropic base64 image source into the data: URL
// OpenAI image parts use. URL sources are returned as-is; ollamaMessages
// drops those, as it does for OpenAI clients.
func imageDataURL(src interface{}) string {
	s, ok := src.(map[string]interface{})
	if !ok {
		return ""
	}
	switch s["type"] {
	case "base64":
		mt, _ := s["media_type"].(string)
		data, _ := s["data"].(string)
		if data == "" {
			return ""
		}
		return "data:" + mt + ";base64," + data
	case "url":
		u, _ := s["url"].(string)
		return u
	}
	return ""
}

// anthropicTools maps client tools to OpenAI function tools. Anthropic
// server tools (web_search, code_execution, …) carry a versioned type and no
// input_schema; no local engine can run them, so they are skipped rather than
// offered as functions the client would then be asked to execute.
func anthropicTools(v interface{}) []interface{} {
	list, _ := v.([]interface{})
	out := make([]interface{}, 0, len(list))
	for _, t := range list {
		tool, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		schema, ok := tool["input_schema"]
		if !ok {
			continue
		}
		fn := map[string]interface{}{"name": tool["name"], "parameters": schema}
		if d, ok := tool["description"].(string); ok && d != "" {
			fn["description"] = d
		}
		out = append(out, map[string]interface{}{"type": "function", "function": fn})
	}
	return out
}

func anthropicToolChoice(v interface{}) interface{} {
	tc, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	switch tc["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]interface{}{
			"type": "function", "function": map[string]interface{}{"name": tc["name"]},
		}
	}
	return nil
}

// ── Response translation ─────────────────────────────────────────────────────

// stopReasonFromFinish maps an OpenAI finish_reason onto Anthropic's
// stop_reason. "stop" is end_turn: OpenAI does not distinguish hitting a stop
// sequence from finishing naturally, and end_turn is what clients expect in
// the common case.
func stopReasonFromFinish(finish string) *string {
	switch finish {
	case "length":
		return &stopMaxTokens
	case "tool_calls", "function_call":
		return &stopToolUse
	}
	return &stopEndTurn
}

// toolInput parses a tool call's JSON-string arguments into the object
// Anthropic's tool_use.input requires. Arguments a model emitted as invalid
// JSON become {} rather than failing the whole response: the client can
// still see which tool was called and report the problem to the model.
func toolInput(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" || !json.Valid([]byte(args)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}

// openAIToAnthropic converts a non-streaming chat completion — either the
// typed body ollamaToOpenAI produced or a relayed OpenAI-compatible map — to
// an Anthropic message. Round-tripping through wire.ChatCompletion gives both
// sources one typed shape to read.
func openAIToAnthropic(payload interface{}, model string) (*AnthropicMessage, error) {
	var cc wire.ChatCompletion
	switch p := payload.(type) {
	case *wire.ChatCompletion:
		cc = *p
	default:
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &cc); err != nil {
			return nil, err
		}
	}

	msg := &AnthropicMessage{
		ID:         newMessageID(),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    []AnthropicContentBlock{},
		Usage:      anthropicUsage(cc.Usage),
		StopReason: &stopEndTurn,
	}
	if len(cc.Choices) == 0 {
		return msg, nil
	}
	choice := cc.Choices[0]
	// Reasoning comes first, as Anthropic orders a thinking block before the
	// answer. The handler removes it when the request did not ask for it.
	if r := choice.Message.ReasoningText(); r != "" {
		msg.Content = append(msg.Content, thinkingBlock(r))
	}
	if text := choice.Message.Content; text != "" {
		msg.Content = append(msg.Content, AnthropicContentBlock{Type: "text", Text: &text})
	}
	for _, tc := range choice.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = newToolUseID()
		}
		msg.Content = append(msg.Content, AnthropicContentBlock{
			Type: "tool_use", ID: id, Name: tc.Function.Name, Input: toolInput(tc.Function.Arguments),
		})
	}
	finish := ""
	if choice.FinishReason != nil {
		finish = *choice.FinishReason
	}
	msg.StopReason = stopReasonFromFinish(finish)
	// Some engines finish with "stop" even after calling tools; the client
	// must still be told to run them.
	if len(choice.Message.ToolCalls) > 0 {
		msg.StopReason = &stopToolUse
	}
	return msg, nil
}

// ── Streaming translation ────────────────────────────────────────────────────

// sseEvent is one Anthropic SSE frame: an event name and its JSON payload.
type sseEvent struct {
	name string
	data interface{}
}

// anthropicStream converts a sequence of OpenAI chat.completion.chunk payloads
// into Anthropic stream events.
//
// The two formats model a reply differently. OpenAI sends flat deltas — some
// text here, a fragment of tool call #i there — while Anthropic sends a
// sequence of explicitly opened and closed content blocks with indices. The
// translator holds the one block that is currently open and closes it when a
// delta of a different kind (or a different tool call) arrives.
type anthropicStream struct {
	id    string
	model string

	started bool
	// thinking: forward the engine's reasoning as thinking blocks; set when
	// the request asked for thinking (thinkingRequested), else it is dropped.
	thinking bool

	// open is the kind of the open block: "" (none), "thinking", "text" or
	// "tool".
	open       string
	blockIndex int // index of the open block; -1 before the first
	toolIndex  int // OpenAI index of the open tool call
	sawTool    bool

	finish string
	usage  AnthropicUsage
}

func newAnthropicStream(model string) *anthropicStream {
	return &anthropicStream{id: newMessageID(), model: model, blockIndex: -1}
}

// start returns message_start. It is emitted before the first upstream chunk
// arrives so the client sees the message open immediately.
func (s *anthropicStream) start() sseEvent {
	s.started = true
	return sseEvent{"message_start", map[string]interface{}{
		"type": "message_start",
		"message": AnthropicMessage{
			ID: s.id, Type: "message", Role: "assistant", Model: s.model,
			Content: []AnthropicContentBlock{},
		},
	}}
}

// streamChunk is the subset of an OpenAI chunk the translator reads.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// The engine's reasoning, under either name (see wire.Message).
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wire.Usage `json:"usage"`
}

// chunk translates one OpenAI chunk payload into zero or more events. A
// payload that does not parse as a chunk produces nothing.
func (s *anthropicStream) chunk(payload []byte) []sseEvent {
	var c streamChunk
	if json.Unmarshal(payload, &c) != nil {
		return nil
	}
	var events []sseEvent
	if c.Usage != nil {
		s.usage = anthropicUsage(*c.Usage)
	}
	for _, ch := range c.Choices {
		reasoning := ch.Delta.ReasoningContent
		if reasoning == "" {
			reasoning = ch.Delta.Reasoning
		}
		if reasoning != "" && s.thinking {
			if s.open != "thinking" {
				events = append(events, s.closeBlock()...)
				s.blockIndex++
				s.open = "thinking"
				events = append(events, sseEvent{"content_block_start", map[string]interface{}{
					"type": "content_block_start", "index": s.blockIndex,
					"content_block": thinkingBlock(""),
				}})
			}
			events = append(events, sseEvent{"content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": s.blockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": reasoning},
			}})
		}
		if ch.Delta.Content != "" {
			if s.open != "text" {
				events = append(events, s.closeBlock()...)
				s.blockIndex++
				s.open = "text"
				empty := ""
				events = append(events, sseEvent{"content_block_start", map[string]interface{}{
					"type": "content_block_start", "index": s.blockIndex,
					"content_block": AnthropicContentBlock{Type: "text", Text: &empty},
				}})
			}
			events = append(events, sseEvent{"content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": s.blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": ch.Delta.Content},
			}})
		}
		for _, tc := range ch.Delta.ToolCalls {
			if s.open != "tool" || s.toolIndex != tc.Index {
				events = append(events, s.closeBlock()...)
				s.blockIndex++
				s.open = "tool"
				s.toolIndex = tc.Index
				s.sawTool = true
				id := tc.ID
				if id == "" {
					id = newToolUseID()
				}
				events = append(events, sseEvent{"content_block_start", map[string]interface{}{
					"type": "content_block_start", "index": s.blockIndex,
					"content_block": AnthropicContentBlock{
						Type: "tool_use", ID: id, Name: tc.Function.Name, Input: json.RawMessage("{}"),
					},
				}})
			}
			if tc.Function.Arguments != "" {
				events = append(events, sseEvent{"content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": s.blockIndex,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				}})
			}
		}
		if ch.FinishReason != nil {
			s.finish = *ch.FinishReason
		}
	}
	return events
}

func (s *anthropicStream) closeBlock() []sseEvent {
	if s.open == "" {
		return nil
	}
	s.open = ""
	return []sseEvent{{"content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": s.blockIndex,
	}}}
}

// end closes the open block and emits message_delta and message_stop.
func (s *anthropicStream) end() []sseEvent {
	events := s.closeBlock()
	stop := stopReasonFromFinish(s.finish)
	if s.sawTool {
		stop = &stopToolUse
	}
	return append(events,
		sseEvent{"message_delta", map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]interface{}{"stop_reason": stop, "stop_sequence": nil},
			"usage": s.usage,
		}},
		sseEvent{"message_stop", map[string]interface{}{"type": "message_stop"}},
	)
}

// estimateTokens approximates a token count at four bytes per token. No local
// engine exposes a tokenizer endpoint the gateway could call, and Claude Code
// uses count_tokens only to decide when to compact context — an estimate on
// the high side of accurate is the safe direction for that.
func estimateTokens(v ...interface{}) int {
	n := 0
	for _, x := range v {
		if x == nil {
			continue
		}
		if b, err := json.Marshal(x); err == nil {
			n += len(b)
		}
	}
	return (n + 3) / 4
}
