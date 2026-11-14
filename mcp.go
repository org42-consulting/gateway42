package main

// MCP tools provided by the backend.
//
// oMLX (and engines built like it, from vllm-mlx) can connect to MCP servers
// itself. It then adds their tools — named "server__tool" — to every chat,
// Responses and Messages request it serves, so the model may call them. But
// it does not run them: the call comes back to the client like any other tool
// call, and the client is expected to run it through the engine's
// POST /v1/mcp/execute. A client that only knows its own tools — Claude
// Desktop, Cowork, Claude Code, any OpenAI SDK app — cannot: it gets a call
// for a tool it never offered, and the turn fails.
//
// The gateway closes that loop. On /v1/chat/completions and /v1/messages,
// streaming or not, a tool call for a backend MCP tool is executed through
// the engine's /v1/mcp/execute, its result is appended to the conversation,
// and the engine is asked again — up to GW42_MCP_MAX_ROUNDS times — until the
// model answers or calls one of the client's own tools. The client sees the
// final answer, and on /v1/messages, when it sends the anthropic-beta
// "mcp-client-…" flag, the calls and results as Anthropic's mcp_tool_use and
// mcp_tool_result blocks. The client's own tools always win a name clash, as
// they do in oMLX.
//
// The engine's MCP endpoints are also relayed — GET /v1/mcp/tools and
// /v1/mcp/servers combined across engines, POST /v1/mcp/execute routed to the
// engine that has the tool — for clients that run the calls themselves
// (for example on /v1/responses, which is passed through unchanged).
//
// Running a backend MCP tool can have side effects (a filesystem or shell
// server). Through the gateway any API key can cause one, just as any caller
// with the engine's own key could. GW42_MCP=off turns all of this off.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// mcpEnabled reports whether backend MCP support is on (GW42_MCP, default on).
func mcpEnabled() bool { return cfg.MCPMode != "off" }

func mcpMaxRounds() int {
	if cfg.MCPMaxRounds > 0 {
		return cfg.MCPMaxRounds
	}
	return 8
}

// ── Tool catalogue ───────────────────────────────────────────────────────────

// mcpCatalog is one engine's MCP tools, name → server.
type mcpCatalog struct {
	tools   map[string]string
	fetched time.Time
}

var mcpCatalogs = struct {
	sync.Mutex
	m map[string]mcpCatalog
}{m: map[string]mcpCatalog{}}

const mcpCatalogTTL = 30 * time.Second

// mcpToolsFor returns the MCP tools an engine exposes to its models, or nil.
// Any engine may expose them; oMLX does when started with an MCP config.
// The list is cached briefly, failures included, so an engine without the
// endpoint costs one probe per TTL rather than one per request.
func mcpToolsFor(e Engine) map[string]string {
	a, ok := e.(*OpenAICompatAdapter)
	if !ok || !mcpEnabled() {
		return nil
	}
	key := a.upstream("/v1/mcp/tools")
	mcpCatalogs.Lock()
	c, cached := mcpCatalogs.m[key]
	mcpCatalogs.Unlock()
	if cached && time.Since(c.fetched) < mcpCatalogTTL {
		return c.tools
	}
	tools, _ := fetchMCPTools(a)
	mcpCatalogs.Lock()
	mcpCatalogs.m[key] = mcpCatalog{tools: tools, fetched: time.Now()}
	mcpCatalogs.Unlock()
	return tools
}

// mcpToolInfo is one entry of an engine's GET /v1/mcp/tools.
type mcpToolInfo struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Server      string                 `json:"server"`
	Parameters  map[string]interface{} `json:"parameters"`
}

func fetchMCPToolList(a *OpenAICompatAdapter) ([]mcpToolInfo, error) {
	req, err := http.NewRequest(http.MethodGet, a.upstream("/v1/mcp/tools"), nil)
	if err != nil {
		return nil, err
	}
	if a.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	}
	resp, err := controlClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, newUpstreamStatusError(resp)
	}
	var body struct {
		Tools []mcpToolInfo `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Tools, nil
}

func fetchMCPTools(a *OpenAICompatAdapter) (map[string]string, error) {
	list, err := fetchMCPToolList(a)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	tools := make(map[string]string, len(list))
	for _, t := range list {
		tools[t.Name] = t.Server
	}
	return tools, nil
}

// clientToolNames is the set of tool names the client itself offered, in
// OpenAI form (tools, or the legacy functions). These are never treated as
// backend MCP tools, even on a name clash.
func clientToolNames(data map[string]interface{}) map[string]bool {
	names := map[string]bool{}
	tools, _ := data["tools"].([]interface{})
	for _, t := range tools {
		tm, _ := t.(map[string]interface{})
		fn, _ := tm["function"].(map[string]interface{})
		if n, _ := fn["name"].(string); n != "" {
			names[n] = true
		}
	}
	fns, _ := data["functions"].([]interface{})
	for _, f := range fns {
		fm, _ := f.(map[string]interface{})
		if n, _ := fm["name"].(string); n != "" {
			names[n] = true
		}
	}
	return names
}

// mcpSession is the per-request view: which calls belong to the backend.
type mcpSession struct {
	adapter *OpenAICompatAdapter
	tools   map[string]string // backend MCP tools, name → server
	client  map[string]bool   // the client's own tools
	userID  int
}

// newMCPSession returns nil when the request cannot involve backend MCP
// tools: MCP is off, the engine has none, or the client disabled tools.
func newMCPSession(e Engine, data map[string]interface{}, userID int) *mcpSession {
	tools := mcpToolsFor(e)
	if len(tools) == 0 {
		return nil
	}
	if tc, _ := data["tool_choice"].(string); tc == "none" {
		return nil
	}
	return &mcpSession{adapter: e.(*OpenAICompatAdapter), tools: tools, client: clientToolNames(data), userID: userID}
}

func (s *mcpSession) isMCP(name string) bool {
	if s == nil || s.client[name] {
		return false
	}
	_, ok := s.tools[name]
	return ok
}

func (s *mcpSession) server(name string) string { return s.tools[name] }

// ── Execution ────────────────────────────────────────────────────────────────

type mcpCall struct {
	ID   string
	Name string
	Args string // JSON, as the model produced it
}

type mcpResult struct {
	Content string
	IsError bool
}

// execute runs one backend MCP tool through the engine. Failures are results,
// not errors: the model is told the tool failed and can recover, as it would
// from a failing client tool.
func (s *mcpSession) execute(ctx context.Context, call mcpCall) mcpResult {
	start := time.Now()
	args := map[string]interface{}{}
	if strings.TrimSpace(call.Args) != "" {
		if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
			return mcpResult{Content: "Invalid tool arguments (not a JSON object): " + err.Error(), IsError: true}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamChatTimeout())
	defer cancel()
	res, err := postJSON(ctx, s.adapter.Type(), "mcp_execute", s.adapter.upstream("/v1/mcp/execute"), s.adapter.cfg.APIKey,
		map[string]interface{}{"tool_name": call.Name, "arguments": args})
	out := mcpResult{}
	switch {
	case err != nil:
		out = mcpResult{Content: "The tool could not be run: " + err.Error(), IsError: true}
	default:
		out.Content = mcpContentText(res["content"])
		out.IsError, _ = res["is_error"].(bool)
		if msg, _ := res["error_message"].(string); msg != "" {
			out.IsError = true
			if out.Content == "" {
				out.Content = msg
			}
		}
	}
	slog.Info("mcp tool executed", "user", s.userID, "engine", s.adapter.Name(), "tool", call.Name,
		"server", s.server(call.Name), "is_error", out.IsError, "ms", time.Since(start).Milliseconds())
	return out
}

// mcpContentText flattens an MCP tool result (a string, a list of content
// items, or an object) to the text a tool message carries.
func mcpContentText(v interface{}) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []interface{}:
		var texts []string
		for _, item := range c {
			if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					texts = append(texts, t)
					continue
				}
			}
			b, _ := json.Marshal(item)
			texts = append(texts, string(b))
		}
		return strings.Join(texts, "\n")
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// executeAll runs a round's calls in order.
func (s *mcpSession) executeAll(ctx context.Context, calls []mcpCall) []mcpResult {
	results := make([]mcpResult, len(calls))
	for i, c := range calls {
		results[i] = s.execute(ctx, c)
	}
	return results
}

// appendMCPRound extends the conversation with one executed round: the
// assistant turn that made the calls, then one tool message per result. The
// caller's slice is not modified.
func appendMCPRound(messages []map[string]interface{}, content string, calls []mcpCall, results []mcpResult) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(messages)+1+len(calls))
	out = append(out, messages...)
	tcs := make([]interface{}, len(calls))
	for i, c := range calls {
		// Re-encoded the way the request translation encodes a tool call's
		// input (json.Marshal, sorted keys), so when a client sends this turn
		// back as mcp_tool_use blocks the engine sees the same bytes and its
		// prefix cache still matches.
		args := "{}"
		var parsed interface{}
		if json.Unmarshal([]byte(c.Args), &parsed) == nil && parsed != nil {
			if b, err := json.Marshal(parsed); err == nil {
				args = string(b)
			}
		}
		tcs[i] = map[string]interface{}{"id": c.ID, "type": "function",
			"function": map[string]interface{}{"name": c.Name, "arguments": args}}
	}
	turn := map[string]interface{}{"role": "assistant", "tool_calls": tcs}
	if content != "" {
		turn["content"] = content
	} else {
		turn["content"] = nil
	}
	out = append(out, turn)
	for i, c := range calls {
		text := results[i].Content
		if results[i].IsError {
			text = "Error: " + text
		}
		out = append(out, map[string]interface{}{"role": "tool", "tool_call_id": c.ID, "content": text})
	}
	return out
}

// mcpRound is one executed round, kept for the response (Anthropic's
// mcp_tool_use / mcp_tool_result blocks) and the audit.
type mcpRound struct {
	Text    string
	Calls   []mcpCall
	Results []mcpResult
}

// ── Non-streaming loop ───────────────────────────────────────────────────────

// chatWithMCP is adapter.Chat that executes backend MCP tool calls. It
// returns the final engine response (OpenAI-shaped, relayed as the engine
// sent it but with usage summed over all rounds and the backend MCP calls
// removed) and the rounds it executed. Without a session it is one Chat.
//
// A round that mixes backend and client tool calls ends the loop: the client
// must run its calls before the model can continue, so the response carries
// only the client's calls and the backend ones are dropped unrun — the model
// can call them again on the next turn.
func chatWithMCP(ctx context.Context, adapter Engine, session *mcpSession, data map[string]interface{},
	messages []map[string]interface{}, firstReq map[string]interface{}) (map[string]interface{}, []mcpRound, error) {

	result, err := adapter.Chat(ctx, firstReq)
	if err != nil || session == nil {
		return result, nil, err
	}
	var rounds []mcpRound
	var usage [3]int // prompt, completion, cached
	for round := 0; ; round++ {
		addUsage(&usage, result)
		msg := firstChoiceMessage(result)
		mcpCalls, clientCalls := session.splitCalls(msg["tool_calls"])
		if len(mcpCalls) == 0 || len(clientCalls) > 0 || round >= mcpMaxRounds() {
			if len(mcpCalls) > 0 {
				keepToolCalls(result, clientCalls)
			}
			setUsage(result, usage)
			return result, rounds, nil
		}
		text, _ := msg["content"].(string)
		results := session.executeAll(ctx, mcpCalls)
		rounds = append(rounds, mcpRound{Text: text, Calls: mcpCalls, Results: results})
		messages = appendMCPRound(messages, text, mcpCalls, results)
		if result, err = adapter.Chat(ctx, buildEngineRequest(adapter, data, messages)); err != nil {
			return nil, rounds, err
		}
	}
}

// splitCalls separates an OpenAI tool_calls list into backend MCP calls and
// the client's own (kept as decoded).
func (s *mcpSession) splitCalls(v interface{}) ([]mcpCall, []interface{}) {
	list, _ := v.([]interface{})
	var mcp []mcpCall
	var client []interface{}
	for i, c := range list {
		cm, _ := c.(map[string]interface{})
		fn, _ := cm["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		if !s.isMCP(name) {
			client = append(client, c)
			continue
		}
		id, _ := cm["id"].(string)
		if id == "" {
			id = fmt.Sprintf("call_mcp_%d_%s", i, randomHex(6))
		}
		args, _ := fn["arguments"].(string)
		if args == "" && fn["arguments"] != nil {
			b, _ := json.Marshal(fn["arguments"])
			args = string(b)
		}
		mcp = append(mcp, mcpCall{ID: id, Name: name, Args: args})
	}
	return mcp, client
}

func firstChoiceMessage(result map[string]interface{}) map[string]interface{} {
	choices, _ := result["choices"].([]interface{})
	if len(choices) == 0 {
		return nil
	}
	ch, _ := choices[0].(map[string]interface{})
	msg, _ := ch["message"].(map[string]interface{})
	return msg
}

// keepToolCalls replaces the first choice's tool calls with calls; with none
// left, the turn is an answer and finishes as "stop".
func keepToolCalls(result map[string]interface{}, calls []interface{}) {
	choices, _ := result["choices"].([]interface{})
	if len(choices) == 0 {
		return
	}
	ch, _ := choices[0].(map[string]interface{})
	msg, _ := ch["message"].(map[string]interface{})
	if msg == nil {
		return
	}
	if len(calls) == 0 {
		delete(msg, "tool_calls")
		if ch["finish_reason"] == "tool_calls" {
			ch["finish_reason"] = "stop"
		}
		return
	}
	msg["tool_calls"] = calls
}

func addUsage(sum *[3]int, result map[string]interface{}) {
	u, _ := result["usage"].(map[string]interface{})
	sum[0] += toInt(u["prompt_tokens"])
	sum[1] += toInt(u["completion_tokens"])
	if d, ok := u["prompt_tokens_details"].(map[string]interface{}); ok {
		sum[2] += toInt(d["cached_tokens"])
	}
}

func setUsage(result map[string]interface{}, sum [3]int) {
	u := map[string]interface{}{"prompt_tokens": sum[0], "completion_tokens": sum[1], "total_tokens": sum[0] + sum[1]}
	if sum[2] > 0 {
		u["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": sum[2]}
	}
	result["usage"] = u
}

// prependRoundText puts what the model said before each executed round in
// front of the final message content, so an OpenAI client's answer reads as
// one turn.
func prependRoundText(result map[string]interface{}, rounds []mcpRound) {
	msg := firstChoiceMessage(result)
	if msg == nil {
		return
	}
	var parts []string
	for _, r := range rounds {
		if strings.TrimSpace(r.Text) != "" {
			parts = append(parts, r.Text)
		}
	}
	if final, _ := msg["content"].(string); final != "" {
		parts = append(parts, final)
	}
	if len(parts) > 0 {
		msg["content"] = strings.Join(parts, "\n\n")
	}
}

// mcpBlocksRequested reports whether an Anthropic client asked for MCP
// content blocks: Anthropic's MCP connector beta flag. Without it the
// executed calls stay invisible — a client that does not expect
// mcp_tool_use blocks may not handle them.
func mcpBlocksRequested(r *http.Request) bool {
	for _, v := range r.Header.Values("anthropic-beta") {
		if strings.Contains(v, "mcp-client") {
			return true
		}
	}
	return false
}

// mcpUseBlock and mcpResultBlock are Anthropic's MCP connector blocks for
// one executed call.
func mcpUseBlock(c mcpCall, server string) AnthropicContentBlock {
	return AnthropicContentBlock{Type: "mcp_tool_use", ID: c.ID, Name: c.Name, ServerName: server, Input: toolInput(c.Args)}
}

func mcpResultBlock(c mcpCall, res mcpResult) AnthropicContentBlock {
	isErr := res.IsError
	text := res.Content
	return AnthropicContentBlock{Type: "mcp_tool_result", ToolUseID: c.ID, IsError: &isErr,
		Content: []AnthropicContentBlock{{Type: "text", Text: &text}}}
}

// mcpRoundBlocks renders executed rounds as Anthropic content: each round's
// text, then — when shown — its calls and results.
func mcpRoundBlocks(rounds []mcpRound, s *mcpSession, show bool) []AnthropicContentBlock {
	var out []AnthropicContentBlock
	for _, r := range rounds {
		if strings.TrimSpace(r.Text) != "" {
			text := r.Text
			out = append(out, AnthropicContentBlock{Type: "text", Text: &text})
		}
		if !show {
			continue
		}
		for i, c := range r.Calls {
			out = append(out, mcpUseBlock(c, s.server(c.Name)), mcpResultBlock(c, r.Results[i]))
		}
	}
	return out
}

// ── Relayed MCP endpoints ────────────────────────────────────────────────────

// mcpEngines lists the OpenAI-compatible engines, the only ones that can
// expose MCP tools.
func mcpEngines() []*OpenAICompatAdapter {
	var out []*OpenAICompatAdapter
	for _, ec := range cachedEngines() {
		if a, ok := cachedAdapter(ec.ID).(*OpenAICompatAdapter); ok {
			out = append(out, a)
		}
	}
	return out
}

// handleMCPTools serves GET /v1/mcp/tools: every engine's MCP tools, each
// tagged with the engine that has it. The first engine wins a duplicate name,
// as it does for execution.
func handleMCPTools(w http.ResponseWriter, r *http.Request) {
	if !mcpEnabled() {
		writeOpenAIError(w, 404, "MCP is turned off on this gateway", "invalid_request_error")
		return
	}
	seen := map[string]bool{}
	tools := []map[string]interface{}{}
	for _, a := range mcpEngines() {
		list, err := fetchMCPToolList(a)
		if err != nil {
			continue
		}
		for _, t := range list {
			if seen[t.Name] {
				continue
			}
			seen[t.Name] = true
			tools = append(tools, map[string]interface{}{"name": t.Name, "description": t.Description,
				"server": t.Server, "parameters": t.Parameters, "engine": a.Name()})
		}
	}
	jsonResponse(w, 200, map[string]interface{}{"tools": tools, "count": len(tools)})
}

// handleMCPServers serves GET /v1/mcp/servers: every engine's MCP servers.
func handleMCPServers(w http.ResponseWriter, r *http.Request) {
	if !mcpEnabled() {
		writeOpenAIError(w, 404, "MCP is turned off on this gateway", "invalid_request_error")
		return
	}
	servers := []interface{}{}
	for _, a := range mcpEngines() {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.upstream("/v1/mcp/servers"), nil)
		if a.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
		}
		resp, err := controlClient.Do(req)
		if err == nil {
			var body struct {
				Servers []map[string]interface{} `json:"servers"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil {
				for _, s := range body.Servers {
					s["engine"] = a.Name()
					servers = append(servers, s)
				}
			}
			_ = resp.Body.Close()
		}
		cancel()
	}
	jsonResponse(w, 200, map[string]interface{}{"servers": servers})
}

// handleMCPExecute serves POST /v1/mcp/execute ({"tool_name", "arguments"}),
// running the tool on the engine that exposes it.
func handleMCPExecute(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		writeOpenAIError(w, 401, "Invalid API key", "authentication_error")
		return
	}
	if !mcpEnabled() {
		writeOpenAIError(w, 404, "MCP is turned off on this gateway", "invalid_request_error")
		return
	}
	body, ok := decodeAPIBody(w, r, writeOpenAIError)
	if !ok {
		return
	}
	name, _ := body["tool_name"].(string)
	if name == "" {
		name, _ = body["tool"].(string)
	}
	if name == "" {
		writeOpenAIError(w, 400, "'tool_name' is required", "invalid_request_error")
		return
	}
	for _, a := range mcpEngines() {
		tools := mcpToolsFor(a)
		if _, ok := tools[name]; !ok {
			continue
		}
		detailFromContext(r).setRouted(a.Name(), a.Type(), name)
		args, _ := json.Marshal(body["arguments"])
		if body["arguments"] == nil {
			args = []byte("{}")
		}
		s := &mcpSession{adapter: a, tools: tools, userID: user.ID}
		res := s.execute(r.Context(), mcpCall{Name: name, Args: string(args)})
		out := map[string]interface{}{"tool_name": name, "content": res.Content, "is_error": res.IsError}
		if res.IsError {
			out["error_message"] = res.Content
		}
		jsonResponse(w, 200, out)
		return
	}
	writeOpenAIError(w, 404, fmt.Sprintf("No engine exposes the MCP tool '%s'", name), "invalid_request_error")
}

// ── Streaming (OpenAI) ───────────────────────────────────────────────────────

// openAIStreamFilter rewrites an OpenAI chunk stream that may span several
// engine streams (one per backend MCP round) into one stream for the client:
//
//   - tool-call deltas for backend MCP tools are taken out and collected;
//   - the client's own tool calls are renumbered 0, 1, … in order, since
//     indexes the engine gave MCP calls never reach the client;
//   - every chunk carries the first round's completion id;
//   - finish_reason is held back, and usage taken out, until the end:
//     finalChunks emits one finishing chunk and one usage chunk summed over
//     all rounds.
type openAIStreamFilter struct {
	s            *mcpSession
	id           string
	model        string
	pending      map[int]*mcpCall
	order        []int
	clientIndex  map[int]int
	nextClient   int
	clientCalled bool
	text         strings.Builder
	usage        [3]int
	finish       string
	sentRole     bool // the opening role chunk goes out once, not per round
}

func newOpenAIStreamFilter(s *mcpSession) *openAIStreamFilter {
	return &openAIStreamFilter{s: s, pending: map[int]*mcpCall{}, clientIndex: map[int]int{}}
}

// filter returns the chunk to send, or nil to send nothing.
func (f *openAIStreamFilter) filter(payload []byte) []byte {
	var c map[string]interface{}
	if json.Unmarshal(payload, &c) != nil {
		return payload
	}
	if c["usage"] != nil {
		addUsage(&f.usage, c)
		delete(c, "usage")
	}
	if id, _ := c["id"].(string); f.id == "" {
		f.id = id
	} else {
		c["id"] = f.id
	}
	if m, _ := c["model"].(string); f.model == "" {
		f.model = m
	}
	choices, _ := c["choices"].([]interface{})
	meaningful := false
	for _, ci := range choices {
		ch, _ := ci.(map[string]interface{})
		if fr, _ := ch["finish_reason"].(string); fr != "" {
			f.finish = fr
		}
		ch["finish_reason"] = nil
		delta, _ := ch["delta"].(map[string]interface{})
		if content, _ := delta["content"].(string); content != "" {
			f.text.WriteString(content)
		}
		if tcs, ok := delta["tool_calls"].([]interface{}); ok {
			var kept []interface{}
			for _, t := range tcs {
				tc, _ := t.(map[string]interface{})
				idx := toInt(tc["index"])
				fn, _ := tc["function"].(map[string]interface{})
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				if p, ok := f.pending[idx]; ok {
					p.Args += args
					continue
				}
				if _, known := f.clientIndex[idx]; !known && f.s.isMCP(name) {
					id, _ := tc["id"].(string)
					if id == "" {
						id = "call_mcp_" + randomHex(8)
					}
					f.pending[idx] = &mcpCall{ID: id, Name: name, Args: args}
					f.order = append(f.order, idx)
					continue
				}
				ci, known := f.clientIndex[idx]
				if !known {
					ci = f.nextClient
					f.nextClient++
					f.clientIndex[idx] = ci
					f.clientCalled = true
				}
				tc["index"] = ci
				kept = append(kept, tc)
			}
			if len(kept) == 0 {
				delete(delta, "tool_calls")
			} else {
				delta["tool_calls"] = kept
			}
		}
		for k, v := range delta {
			if k == "role" || v == nil || v == "" {
				continue
			}
			meaningful = true
		}
		if delta["role"] != nil {
			if f.sentRole {
				delete(delta, "role")
			} else {
				f.sentRole = true
				meaningful = true // the opening role chunk
			}
		}
		if ch["logprobs"] != nil {
			meaningful = true
		}
	}
	if !meaningful {
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return payload
	}
	return b
}

// takeRound returns the round's backend MCP calls, its text, and whether the
// client's own tools were called.
func (f *openAIStreamFilter) takeRound() ([]mcpCall, string, bool) {
	calls := make([]mcpCall, 0, len(f.order))
	for _, i := range f.order {
		calls = append(calls, *f.pending[i])
	}
	return calls, f.text.String(), f.clientCalled
}

// nextRound clears the per-round state before the next engine stream.
func (f *openAIStreamFilter) nextRound() {
	f.pending, f.order = map[int]*mcpCall{}, nil
	f.clientIndex = map[int]int{}
	f.text.Reset()
	f.finish = ""
}

// finalChunks is the held-back ending: the finish_reason (tool_calls only if
// the client has calls to run) and the usage summed over all rounds.
func (f *openAIStreamFilter) finalChunks() [][]byte {
	finish := f.finish
	if finish == "" || (finish == "tool_calls" && !f.clientCalled) {
		finish = "stop"
	}
	base := func() map[string]interface{} {
		return map[string]interface{}{"id": f.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": f.model}
	}
	end := base()
	end["choices"] = []interface{}{map[string]interface{}{"index": 0, "delta": map[string]interface{}{}, "finish_reason": finish}}
	u := map[string]interface{}{}
	setUsage(u, f.usage)
	usage := base()
	usage["choices"] = []interface{}{}
	usage["usage"] = u["usage"]
	a, _ := json.Marshal(end)
	b, _ := json.Marshal(usage)
	return [][]byte{a, b}
}

func (f *openAIStreamFilter) tokens() (int, int) { return f.usage[0], f.usage[1] }
