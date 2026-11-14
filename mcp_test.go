package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOMLX is an engine built like oMLX: it has an MCP server "fs" with one
// tool, fs__read, which its model may call, and answers /v1/mcp/execute. The
// model's behaviour is scripted per test by script(round, messages).
type fakeOMLX struct {
	t        *testing.T
	mu       sync.Mutex
	executed []map[string]interface{}
	requests []map[string]interface{}
	script   func(round int, messages []interface{}) fakeTurn
	srv      *httptest.Server
}

// fakeTurn is one model reply: text, then tool calls.
type fakeTurn struct {
	text  string
	calls []fakeCall
	usage [2]int
}

type fakeCall struct{ id, name, args string }

func newFakeOMLX(t *testing.T, script func(int, []interface{}) fakeTurn) *fakeOMLX {
	f := &fakeOMLX{t: t, script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOMLX) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/mcp/tools":
		w.Write([]byte(`{"tools":[{"name":"fs__read","description":"Read a file","server":"fs","parameters":{"type":"object"}}],"count":1}`))
	case "/v1/mcp/servers":
		w.Write([]byte(`{"servers":[{"name":"fs","state":"connected","transport":"stdio","tools_count":1}]}`))
	case "/v1/mcp/execute":
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.executed = append(f.executed, body)
		f.mu.Unlock()
		args, _ := body["arguments"].(map[string]interface{})
		fmt.Fprintf(w, `{"tool_name":%q,"content":[{"type":"text","text":"contents of %v"}],"is_error":false}`, body["tool_name"], args["path"])
	case "/v1/chat/completions":
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		round := len(f.requests)
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		msgs, _ := req["messages"].([]interface{})
		turn := f.script(round, msgs)
		if stream, _ := req["stream"].(bool); stream {
			f.writeStream(w, round, turn)
			return
		}
		f.writeJSON(w, round, turn)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOMLX) writeJSON(w http.ResponseWriter, round int, turn fakeTurn) {
	msg := map[string]interface{}{"role": "assistant", "content": turn.text}
	finish := "stop"
	if len(turn.calls) > 0 {
		var tcs []interface{}
		for _, c := range turn.calls {
			tcs = append(tcs, map[string]interface{}{"id": c.id, "type": "function",
				"function": map[string]interface{}{"name": c.name, "arguments": c.args}})
		}
		msg["tool_calls"] = tcs
		finish = "tool_calls"
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": fmt.Sprintf("chatcmpl-%d", round), "object": "chat.completion", "model": "m",
		"choices": []interface{}{map[string]interface{}{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]interface{}{"prompt_tokens": turn.usage[0], "completion_tokens": turn.usage[1]},
	})
}

func (f *fakeOMLX) writeStream(w http.ResponseWriter, round int, turn fakeTurn) {
	w.Header().Set("Content-Type", "text/event-stream")
	id := fmt.Sprintf("chatcmpl-%d", round)
	chunk := func(delta map[string]interface{}, finish interface{}) {
		b, _ := json.Marshal(map[string]interface{}{"id": id, "object": "chat.completion.chunk", "model": "m",
			"choices": []interface{}{map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	chunk(map[string]interface{}{"role": "assistant", "content": ""}, nil)
	if turn.text != "" {
		chunk(map[string]interface{}{"content": turn.text}, nil)
	}
	for i, c := range turn.calls {
		chunk(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": i, "id": c.id, "type": "function",
			"function": map[string]interface{}{"name": c.name, "arguments": ""}}}}, nil)
		half := len(c.args) / 2 // arguments arrive in pieces
		for _, part := range []string{c.args[:half], c.args[half:]} {
			chunk(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": i,
				"function": map[string]interface{}{"arguments": part}}}}, nil)
		}
	}
	finish := "stop"
	if len(turn.calls) > 0 {
		finish = "tool_calls"
	}
	chunk(map[string]interface{}{}, finish)
	b, _ := json.Marshal(map[string]interface{}{"id": id, "object": "chat.completion.chunk", "choices": []interface{}{},
		"usage": map[string]interface{}{"prompt_tokens": turn.usage[0], "completion_tokens": turn.usage[1]}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
}

// readThenAnswer is the common script: call fs__read once, then answer.
func readThenAnswer(round int, msgs []interface{}) fakeTurn {
	last, _ := msgs[len(msgs)-1].(map[string]interface{})
	if last["role"] == "tool" {
		return fakeTurn{text: "The file says hi.", usage: [2]int{20, 4}}
	}
	return fakeTurn{text: "Let me look.", calls: []fakeCall{{"call_1", "fs__read", `{"path":"a.txt"}`}}, usage: [2]int{10, 5}}
}

// routeTo makes engine the only one, holding model "m".
func routeTo(t *testing.T, f *fakeOMLX) func(path, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1<<20)
	foSeedUser(t, "mcp-user", "sk-gw-mcp", 6000)
	ec := EngineConfig{ID: 1, Name: "omlx", Type: EngineOpenAICompat, BaseURL: f.srv.URL}
	engineCachePtr.Store(&engineCache{engines: []EngineConfig{ec},
		adapters: map[int]Engine{1: &OpenAICompatAdapter{cfg: ec, concurrencyLimiter: newConcurrencyLimiter(4)}}})
	storeRouteIndex(&routeIndex{byModel: map[string][]int{"m": {1}}, built: time.Now(), probed: true}, routeIndexGeneration())
	t.Cleanup(invalidateRouteIndex)
	return func(path, body string, headers ...string) *httptest.ResponseRecorder {
		method := http.MethodPost
		if body == "" {
			method = http.MethodGet
		}
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sk-gw-mcp")
		r.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		setupRouter().ServeHTTP(rec, r)
		return rec
	}
}

func (f *fakeOMLX) executions() []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]interface{}(nil), f.executed...)
}

// ── OpenAI chat completions ──────────────────────────────────────────────────

func TestMCPChatNonStreaming(t *testing.T) {
	f := newFakeOMLX(t, readThenAnswer)
	post := routeTo(t, f)
	rec := post("/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"what is in a.txt?"}]}`)
	if rec.Code != 200 {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message      map[string]interface{} `json:"message"`
			FinishReason string                 `json:"finish_reason"`
		}
		Usage map[string]int
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	msg := resp.Choices[0].Message
	if msg["content"] != "Let me look.\n\nThe file says hi." || msg["tool_calls"] != nil || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("message = %v finish %s", msg, resp.Choices[0].FinishReason)
	}
	if resp.Usage["prompt_tokens"] != 30 || resp.Usage["completion_tokens"] != 9 {
		t.Errorf("usage = %v, want both rounds summed", resp.Usage)
	}
	ex := f.executions()
	if len(ex) != 1 || ex[0]["tool_name"] != "fs__read" || ex[0]["arguments"].(map[string]interface{})["path"] != "a.txt" {
		t.Fatalf("executed = %v", ex)
	}
	// The second engine request carries the call and its result.
	second, _ := json.Marshal(f.requests[1]["messages"])
	for _, want := range []string{
		`"tool_calls":[{"function":{"arguments":"{\"path\":\"a.txt\"}","name":"fs__read"},"id":"call_1","type":"function"}]`,
		`{"content":"contents of a.txt","role":"tool","tool_call_id":"call_1"}`,
	} {
		if !strings.Contains(string(second), want) {
			t.Errorf("second request missing %s\n%s", want, second)
		}
	}
}

func TestMCPChatStreaming(t *testing.T) {
	f := newFakeOMLX(t, readThenAnswer)
	post := routeTo(t, f)
	rec := post("/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"read a.txt"}]}`)
	out := rec.Body.String()
	if rec.Code != 200 || strings.Contains(out, "fs__read") {
		t.Fatalf("backend MCP call leaked to the client or failed (%d):\n%s", rec.Code, out)
	}
	var text strings.Builder
	var finishes []string
	var usage map[string]interface{}
	ids := map[string]bool{}
	roles := 0
	for _, line := range strings.Split(out, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var c map[string]interface{}
		json.Unmarshal([]byte(payload), &c)
		ids[c["id"].(string)] = true
		if u, ok := c["usage"].(map[string]interface{}); ok {
			usage = u
		}
		for _, ch := range c["choices"].([]interface{}) {
			chm := ch.(map[string]interface{})
			d := chm["delta"].(map[string]interface{})
			if s, _ := d["content"].(string); s != "" {
				text.WriteString(s)
			}
			if d["role"] != nil {
				roles++
			}
			if fr, _ := chm["finish_reason"].(string); fr != "" {
				finishes = append(finishes, fr)
			}
		}
	}
	if text.String() != "Let me look.The file says hi." {
		t.Errorf("text = %q", text.String())
	}
	if len(finishes) != 1 || finishes[0] != "stop" || len(ids) != 1 || roles != 1 {
		t.Errorf("finishes %v, ids %v, role chunks %d: want one stop, one id, one role", finishes, ids, roles)
	}
	if usage["prompt_tokens"] != 30.0 || usage["completion_tokens"] != 9.0 {
		t.Errorf("usage = %v", usage)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") || len(f.executions()) != 1 {
		t.Errorf("executions %d, tail %q", len(f.executions()), out[max(0, len(out)-30):])
	}
}

// The client's own tools win a name clash, and a round that calls the client's
// tools ends the loop with only those calls, renumbered from 0.
func TestMCPClientToolsWinAndMixedRoundsStop(t *testing.T) {
	f := newFakeOMLX(t, func(round int, msgs []interface{}) fakeTurn {
		return fakeTurn{calls: []fakeCall{{"c1", "fs__read", `{"path":"x"}`}, {"c2", "bash", `{"cmd":"ls"}`}}, usage: [2]int{5, 5}}
	})
	post := routeTo(t, f)
	// fs__read is offered by the client itself: it is the client's to run.
	rec := post("/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}],
		"tools":[{"type":"function","function":{"name":"fs__read","parameters":{}}},{"type":"function","function":{"name":"bash","parameters":{}}}]}`)
	if !strings.Contains(rec.Body.String(), `"name":"fs__read"`) || len(f.executions()) != 0 {
		t.Fatalf("client's fs__read was executed or dropped: %s", rec.Body.String())
	}

	// Without the clash: fs__read is the backend's, bash the client's.
	rec = post("/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}],
		"tools":[{"type":"function","function":{"name":"bash","parameters":{}}}]}`)
	out := rec.Body.String()
	if strings.Contains(out, "fs__read") || !strings.Contains(out, `"name":"bash"`) || len(f.executions()) != 0 {
		t.Fatalf("mixed round: %s (executions %d)", out, len(f.executions()))
	}
	if !strings.Contains(out, `"index":0`) || strings.Contains(out, `"index":1,"`) {
		t.Errorf("client call not renumbered to index 0:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish should be tool_calls for the client's call:\n%s", out)
	}
}

func TestMCPRoundLimit(t *testing.T) {
	prev := cfg.MCPMaxRounds
	cfg.MCPMaxRounds = 2
	t.Cleanup(func() { cfg.MCPMaxRounds = prev })
	f := newFakeOMLX(t, func(round int, msgs []interface{}) fakeTurn {
		return fakeTurn{text: "again", calls: []fakeCall{{fmt.Sprintf("c%d", round), "fs__read", `{"path":"loop"}`}}, usage: [2]int{1, 1}}
	})
	post := routeTo(t, f)
	rec := post("/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if n := len(f.executions()); n != 2 {
		t.Errorf("executed %d rounds, want the limit of 2", n)
	}
	if strings.Contains(rec.Body.String(), "fs__read") || !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Errorf("response after the limit: %s", rec.Body.String())
	}
}

func TestMCPOff(t *testing.T) {
	prev := cfg.MCPMode
	cfg.MCPMode = "off"
	t.Cleanup(func() { cfg.MCPMode = prev })
	f := newFakeOMLX(t, readThenAnswer)
	post := routeTo(t, f)
	rec := post("/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if !strings.Contains(rec.Body.String(), `"name":"fs__read"`) || len(f.executions()) != 0 {
		t.Errorf("with GW42_MCP=off the call must reach the client unrun: %s", rec.Body.String())
	}
	if rec := post("/v1/mcp/tools", ""); rec.Code != 404 {
		t.Errorf("/v1/mcp/tools with MCP off: %d", rec.Code)
	}
}

// An engine without MCP (no /v1/mcp/tools) behaves exactly as before.
func TestNoMCPEngineUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"x","type":"function","function":{"name":"fs__read","arguments":"{}"}}]}}]}`))
	}))
	defer srv.Close()
	post := routeTo(t, &fakeOMLX{srv: srv})
	rec := post("/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if !strings.Contains(rec.Body.String(), `"name":"fs__read"`) {
		t.Errorf("tool call from an engine without MCP was altered: %s", rec.Body.String())
	}
}

// ── Anthropic messages ───────────────────────────────────────────────────────

func TestMCPMessagesNonStreaming(t *testing.T) {
	f := newFakeOMLX(t, readThenAnswer)
	post := routeTo(t, f)
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"read a.txt"}]}`

	// Without the MCP beta: the answer only, as text.
	rec := post("/v1/messages", body)
	var msg AnthropicMessage
	json.Unmarshal(rec.Body.Bytes(), &msg)
	got, _ := json.Marshal(msg.Content)
	if string(got) != `[{"type":"text","text":"Let me look."},{"type":"text","text":"The file says hi."}]` || *msg.StopReason != "end_turn" {
		t.Errorf("content = %s stop %s", got, *msg.StopReason)
	}
	if msg.Usage.InputTokens != 30 || msg.Usage.OutputTokens != 9 {
		t.Errorf("usage = %+v", msg.Usage)
	}

	// With it: the call and result as Anthropic's MCP blocks.
	rec = post("/v1/messages", body, "anthropic-beta", "mcp-client-2025-11-20")
	var shown AnthropicMessage // fresh: Unmarshal into msg would keep its old fields
	json.Unmarshal(rec.Body.Bytes(), &shown)
	got, _ = json.Marshal(shown.Content)
	if string(got) != `[{"type":"text","text":"Let me look."},`+
		`{"type":"mcp_tool_use","id":"call_1","name":"fs__read","input":{"path":"a.txt"},"server_name":"fs"},`+
		`{"type":"mcp_tool_result","tool_use_id":"call_1","is_error":false,"content":[{"type":"text","text":"contents of a.txt"}]},`+
		`{"type":"text","text":"The file says hi."}]` {
		t.Errorf("content with MCP blocks = %s", got)
	}
	for _, want := range []string{
		`{"type":"mcp_tool_use","id":"call_1","name":"fs__read","input":{"path":"a.txt"},"server_name":"fs"}`,
		`{"type":"mcp_tool_result","tool_use_id":"call_1","is_error":false,"content":[{"type":"text","text":"contents of a.txt"}]}`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("content missing %s\n%s", want, got)
		}
	}
}

func TestMCPMessagesStreaming(t *testing.T) {
	f := newFakeOMLX(t, readThenAnswer)
	post := routeTo(t, f)
	rec := post("/v1/messages", `{"model":"m","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"read a.txt"}]}`,
		"anthropic-beta", "mcp-client-2025-11-20")
	out := rec.Body.String()
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("backend MCP call streamed as a client tool_use:\n%s", out)
	}
	for _, want := range []string{
		`"text":"Let me look."`,
		`"content_block":{"type":"mcp_tool_use","id":"call_1","name":"fs__read","input":{"path":"a.txt"},"server_name":"fs"}`,
		`"content_block":{"type":"mcp_tool_result","tool_use_id":"call_1","is_error":false`,
		`"text":"The file says hi."`,
		`"stop_reason":"end_turn"`,
		`"input_tokens":30`, `"output_tokens":9`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %s", want)
		}
	}
	if strings.Count(out, "event: message_start") != 1 || strings.Count(out, "event: message_stop") != 1 {
		t.Errorf("rounds must share one message:\n%s", out)
	}
	// Block indexes keep counting across rounds.
	for i := 0; i <= 3; i++ {
		if !strings.Contains(out, fmt.Sprintf(`"index":%d`, i)) {
			t.Errorf("block index %d missing:\n%s", i, out)
		}
	}
}

// A client that received MCP blocks sends them back; the engine gets the
// same call/result sequence it saw when the gateway ran the tool.
func TestMCPBlocksInHistory(t *testing.T) {
	body := decodeJSON(t, `{"model":"m","messages":[
		{"role":"user","content":"read a.txt"},
		{"role":"assistant","content":[
			{"type":"text","text":"Let me look."},
			{"type":"mcp_tool_use","id":"call_1","name":"fs__read","server_name":"fs","input":{"path":"a.txt"}},
			{"type":"mcp_tool_result","tool_use_id":"call_1","is_error":false,"content":[{"type":"text","text":"contents of a.txt"}]},
			{"type":"text","text":"The file says hi."}]},
		{"role":"user","content":"thanks"}]}`).(map[string]interface{})
	_, raw := anthropicToOpenAI(body)
	got, _ := json.Marshal(sanitizeMessages(raw))
	want := `[{"content":"read a.txt","role":"user"},` +
		`{"content":"Let me look.","role":"assistant","tool_calls":[{"function":{"arguments":"{\"path\":\"a.txt\"}","name":"fs__read"},"id":"call_1","type":"function"}]},` +
		`{"content":"contents of a.txt","role":"tool","tool_call_id":"call_1"},` +
		`{"content":"The file says hi.","role":"assistant"},` +
		`{"content":"thanks","role":"user"}]`
	if string(got) != want {
		t.Fatalf("history:\n got %s\nwant %s", got, want)
	}
	// The same turn as the gateway appended it while executing.
	appended := appendMCPRound(nil, "Let me look.", []mcpCall{{ID: "call_1", Name: "fs__read", Args: `{ "path" : "a.txt" }`}},
		[]mcpResult{{Content: "contents of a.txt"}})
	a, _ := json.Marshal(sanitizeMessages(toInterfaces(appended)))
	if !strings.Contains(string(got), strings.Trim(string(a), "[]")) {
		t.Errorf("appended round differs from its history form:\n%s\n%s", a, got)
	}
}

func toInterfaces(ms []map[string]interface{}) []interface{} {
	out := make([]interface{}, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

// ── Relayed MCP endpoints ────────────────────────────────────────────────────

func TestMCPEndpoints(t *testing.T) {
	f := newFakeOMLX(t, readThenAnswer)
	post := routeTo(t, f)
	rec := post("/v1/mcp/tools", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"fs__read"`) || !strings.Contains(rec.Body.String(), `"engine":"omlx"`) || !strings.Contains(rec.Body.String(), `"count":1`) {
		t.Errorf("tools: %d %s", rec.Code, rec.Body.String())
	}
	rec = post("/v1/mcp/servers", "")
	if !strings.Contains(rec.Body.String(), `"name":"fs"`) || !strings.Contains(rec.Body.String(), `"engine":"omlx"`) {
		t.Errorf("servers: %s", rec.Body.String())
	}
	rec = post("/v1/mcp/execute", `{"tool_name":"fs__read","arguments":{"path":"b.txt"}}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"contents of b.txt"`) || !strings.Contains(rec.Body.String(), `"is_error":false`) {
		t.Errorf("execute: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/v1/mcp/execute", `{"tool_name":"nope__x"}`); rec.Code != 404 {
		t.Errorf("unknown tool: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/v1/mcp/execute", `{"arguments":{}}`); rec.Code != 400 {
		t.Errorf("missing tool_name: %d", rec.Code)
	}
	// Without a key, nothing.
	r := httptest.NewRequest(http.MethodGet, "/v1/mcp/tools", nil)
	w := httptest.NewRecorder()
	setupRouter().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("unauthenticated /v1/mcp/tools: %d", w.Code)
	}
}

func TestMCPContentText(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{`"plain"`, "plain"},
		{`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
		{`[{"type":"image","data":"x"}]`, `{"data":"x","type":"image"}`},
		{`{"k":1}`, `{"k":1}`},
		{`null`, ""},
	} {
		var v interface{}
		json.Unmarshal([]byte(c.in), &v)
		if got := mcpContentText(v); got != c.want {
			t.Errorf("mcpContentText(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A failing tool is a result the model sees, not a failed request.
func TestMCPToolFailureIsAResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/mcp/execute":
			w.WriteHeader(500)
			io.WriteString(w, `{"detail":"server crashed"}`)
		}
	}))
	defer srv.Close()
	s := &mcpSession{adapter: &OpenAICompatAdapter{cfg: EngineConfig{BaseURL: srv.URL}}, tools: map[string]string{"fs__read": "fs"}}
	res := s.execute(t.Context(), mcpCall{Name: "fs__read", Args: `{}`})
	if !res.IsError || !strings.Contains(res.Content, "could not be run") {
		t.Errorf("result = %+v", res)
	}
	if res := s.execute(t.Context(), mcpCall{Name: "fs__read", Args: `not json`}); !res.IsError {
		t.Errorf("invalid arguments: %+v", res)
	}
}
