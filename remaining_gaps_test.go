package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// engineFixture routes model to one real adapter of engineType backed by
// handler, and returns a poster that sends a request through the full router.
func engineFixture(t *testing.T, engineType, model string, handler http.HandlerFunc) func(method, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	withSessionStore(t)
	withTestDB(t)
	key := "sk-gw-gap3-" + engineType
	foSeedUser(t, "gap3-"+engineType, key, 6000)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ec := EngineConfig{ID: 1, Name: "gap3", Type: engineType, BaseURL: srv.URL}
	var a Engine = &OpenAICompatAdapter{cfg: ec, concurrencyLimiter: newConcurrencyLimiter(4)}
	if engineType == EngineOllama {
		a = &OllamaAdapter{cfg: ec, concurrencyLimiter: newConcurrencyLimiter(4)}
	}
	engineCachePtr.Store(&engineCache{engines: []EngineConfig{ec}, adapters: map[int]Engine{1: a}})
	storeRouteIndex(&routeIndex{byModel: map[string][]int{model: {1}}, built: time.Now(), probed: true}, routeIndexGeneration())
	t.Cleanup(invalidateRouteIndex)
	return func(method, path, contentType, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		setupRouter().ServeHTTP(rec, r)
		return rec
	}
}

// ── Ollama request mapping ───────────────────────────────────────────────────

func TestOllamaSamplerOptions(t *testing.T) {
	req := openAIToOllama(map[string]interface{}{"model": "m", "top_k": 40.0, "min_p": 0.05, "repetition_penalty": 1.1}, nil)
	opts := req["options"].(map[string]interface{})
	if opts["top_k"] != 40.0 || opts["min_p"] != 0.05 || opts["repeat_penalty"] != 1.1 {
		t.Fatalf("options = %v", opts)
	}
}

func TestOllamaToolChoice(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "read"}},
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "write"}},
	}
	offered := func(choice interface{}) []interface{} {
		data := map[string]interface{}{"model": "m", "tools": tools}
		if choice != nil {
			data["tool_choice"] = choice
		}
		got, _ := openAIToOllama(data, nil)["tools"].([]interface{})
		return got
	}
	if n := len(offered(nil)); n != 2 {
		t.Errorf("no tool_choice: %d tools, want 2", n)
	}
	if n := len(offered("required")); n != 2 {
		t.Errorf("required: %d tools, want 2", n)
	}
	if got := offered("none"); got != nil {
		t.Errorf("none: tools = %v, want none", got)
	}
	named := offered(map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "write"}})
	if len(named) != 1 || named[0].(map[string]interface{})["function"].(map[string]interface{})["name"] != "write" {
		t.Errorf("named: tools = %v, want only write", named)
	}
}

func TestOllamaLegacyFunctions(t *testing.T) {
	data := map[string]interface{}{
		"model":         "m",
		"functions":     []interface{}{map[string]interface{}{"name": "f1"}, map[string]interface{}{"name": "f2"}},
		"function_call": map[string]interface{}{"name": "f2"},
	}
	req := openAIToOllama(data, nil)
	tools, _ := req["tools"].([]interface{})
	if len(tools) != 1 || tools[0].(map[string]interface{})["function"].(map[string]interface{})["name"] != "f2" {
		t.Fatalf("tools = %v, want f2 only", req["tools"])
	}
	if _, mutated := data["tool_choice"]; mutated {
		t.Error("the caller's request was modified")
	}

	// History: an assistant function_call and the "function" result message.
	raw := decodeJSON(t, `[
		{"role":"assistant","content":null,"function_call":{"name":"f1","arguments":"{\"a\":1}"}},
		{"role":"function","name":"f1","content":"done"}
	]`).([]interface{})
	clean := sanitizeMessages(raw)
	if clean[0]["content"] != nil {
		t.Errorf("legacy call turn content = %#v, want null", clean[0]["content"])
	}
	om := ollamaMessages(clean)
	calls, _ := om[0]["tool_calls"].([]map[string]interface{})
	if len(calls) != 1 || calls[0]["function"].(map[string]interface{})["name"] != "f1" {
		t.Errorf("assistant tool_calls = %v", om[0]["tool_calls"])
	}
	if om[1]["role"] != "tool" || om[1]["tool_name"] != "f1" {
		t.Errorf("function result = %v, want a tool message named f1", om[1])
	}
}

func TestOllamaLogprobs(t *testing.T) {
	req := openAIToOllama(map[string]interface{}{"model": "m", "logprobs": true, "top_logprobs": 3.0}, nil)
	if req["logprobs"] != true || req["top_logprobs"] != 3.0 {
		t.Fatalf("request = %v", req)
	}
	lp := []interface{}{map[string]interface{}{"token": "hi", "logprob": -0.1}}
	cc := ollamaToOpenAI(map[string]interface{}{"done": true, "message": map[string]interface{}{"content": "hi"}, "logprobs": lp})
	b, _ := json.Marshal(cc.Choices[0])
	if !strings.Contains(string(b), `"logprobs":{"content":[{"logprob":-0.1,"token":"hi"}]}`) {
		t.Errorf("choice = %s", b)
	}
	chunk := formatStreamChunk(map[string]interface{}{"message": map[string]interface{}{"content": "hi"}, "logprobs": lp}, "id", false)
	if chunk.Choices[0].Logprobs == nil || len(chunk.Choices[0].Logprobs.Content) != 1 {
		t.Errorf("chunk logprobs = %+v", chunk.Choices[0].Logprobs)
	}
	// Not asked, not present: authored bodies unchanged.
	b, _ = json.Marshal(ollamaToOpenAI(map[string]interface{}{"done": true, "message": map[string]interface{}{"content": "hi"}}))
	if strings.Contains(string(b), "logprobs") {
		t.Errorf("logprobs without logprobs: %s", b)
	}
}

func TestOllamaRejectsNGreaterThanOne(t *testing.T) {
	called := false
	post := engineFixture(t, EngineOllama, "m", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"x"},"done":true}`))
	})
	rec := post("POST", "/v1/chat/completions", "application/json", `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`)
	var e struct{ Error struct{ Message string } }
	json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != 400 || !strings.Contains(e.Error.Message, "n > 1") || called {
		t.Fatalf("n=2 → %d %s (engine called: %v)", rec.Code, rec.Body.String(), called)
	}
	if rec := post("POST", "/v1/chat/completions", "application/json", `{"model":"m","n":1,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("n=1 → %d %s", rec.Code, rec.Body.String())
	}
}

// ── OpenAI-compatible parameters ─────────────────────────────────────────────

func TestNewParamsForwardedAndLocalOnesWithheldFromOpenAI(t *testing.T) {
	data := map[string]interface{}{"model": "m", "verbosity": "low", "metadata": map[string]interface{}{"a": "b"},
		"store": false, "functions": []interface{}{}, "top_k": 20.0, "min_p": 0.1, "repetition_penalty": 1.05,
		"thinking_budget": 512.0, "chat_template_kwargs": map[string]interface{}{"enable_thinking": true}}
	local := &OpenAICompatAdapter{cfg: EngineConfig{BaseURL: "http://127.0.0.1:8000"}}
	req := buildEngineRequest(local, data, nil)
	for _, k := range []string{"verbosity", "metadata", "store", "functions", "top_k", "min_p", "repetition_penalty", "thinking_budget", "chat_template_kwargs"} {
		if _, ok := req[k]; !ok {
			t.Errorf("local engine missing %s", k)
		}
	}
	openai := &OpenAICompatAdapter{cfg: EngineConfig{BaseURL: "https://api.openai.com/v1"}}
	req = buildEngineRequest(openai, data, nil)
	for _, k := range localOnlyParams {
		if _, ok := req[k]; ok {
			t.Errorf("api.openai.com got local-only %s", k)
		}
	}
	if req["verbosity"] != "low" || req["store"] != false {
		t.Errorf("api.openai.com lost OpenAI's own params: %v", req)
	}
}

func TestAnthropicTopKAndThinkingBudget(t *testing.T) {
	data, _ := anthropicToOpenAI(decodeJSON(t, `{"model":"m","top_k":40,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[]}`).(map[string]interface{}))
	if data["top_k"] != 40.0 || data["thinking_budget"] != 2048 {
		t.Fatalf("top_k=%v thinking_budget=%v", data["top_k"], data["thinking_budget"])
	}
	data, _ = anthropicToOpenAI(decodeJSON(t, `{"model":"m","thinking":{"type":"disabled","budget_tokens":2048},"messages":[]}`).(map[string]interface{}))
	if _, ok := data["thinking_budget"]; ok {
		t.Error("thinking_budget sent with thinking disabled")
	}
	// Ollama takes top_k as an option.
	if opts := openAIToOllama(map[string]interface{}{"model": "m", "top_k": 40.0}, nil)["options"].(map[string]interface{}); opts["top_k"] != 40.0 {
		t.Errorf("Ollama options = %v", opts)
	}
}

// ── Ollama context window ────────────────────────────────────────────────────

func withOllamaNumCtx(t *testing.T, n int) {
	t.Helper()
	prev := cfg.OllamaNumCtx
	cfg.OllamaNumCtx = n
	t.Cleanup(func() { cfg.OllamaNumCtx = prev })
}

func TestParseOllamaShow(t *testing.T) {
	info := parseOllamaShow(map[string]interface{}{"qwen3.context_length": 40960.0, "general.architecture": "qwen3"},
		"stop \"<|im_end|>\"\nnum_ctx 16384\ntemperature 0.6")
	if info.native != 40960 || info.modelfileCtx != 16384 {
		t.Fatalf("info = %+v", info)
	}
	withOllamaNumCtx(t, 0)
	if n := info.effectiveContext(); n != 16384 {
		t.Errorf("no gateway num_ctx: %d, want the Modelfile's 16384", n)
	}
	withOllamaNumCtx(t, 65536)
	if n := info.effectiveContext(); n != 40960 {
		t.Errorf("num_ctx above the model maximum: %d, want clamped to 40960", n)
	}
	withOllamaNumCtx(t, 8192)
	if n := info.effectiveContext(); n != 8192 {
		t.Errorf("num_ctx 8192: %d", n)
	}
}

func ollamaShowServer(t *testing.T, native int, seen *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*seen = append(*seen, r.URL.Path+" "+string(b))
		mu.Unlock()
		switch r.URL.Path {
		case "/api/show":
			json.NewEncoder(w).Encode(map[string]interface{}{"model_info": map[string]interface{}{"llama.context_length": native}, "parameters": ""})
		case "/api/chat":
			w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOllamaNumCtxSentAndClamped(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := ollamaShowServer(t, 8192, &seen, &mu)
	a := &OllamaAdapter{cfg: EngineConfig{BaseURL: srv.URL + "/numctx"}}
	a.cfg.BaseURL = srv.URL

	withOllamaNumCtx(t, 0)
	req := buildEngineRequest(a, map[string]interface{}{"model": "numctx-a"}, nil)
	if opts, _ := req["options"].(map[string]interface{}); opts["num_ctx"] != nil {
		t.Errorf("num_ctx sent without GW42_OLLAMA_NUM_CTX: %v", opts)
	}

	withOllamaNumCtx(t, 32768)
	req = buildEngineRequest(a, map[string]interface{}{"model": "numctx-b", "temperature": 0.2}, nil)
	opts := req["options"].(map[string]interface{})
	if opts["num_ctx"] != 8192 || opts["temperature"] != 0.2 {
		t.Errorf("options = %v, want num_ctx clamped to the model's 8192 beside temperature", opts)
	}
}

func TestListModelsReportsOllamaContextWindow(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	resetProbeCache(t)
	withOllamaNumCtx(t, 0)
	foSeedUser(t, "octx-user", "sk-gw-octx", 60)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		params := ""
		if req["model"] == "ctx-modelfile" {
			params = "num_ctx 24576"
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"model_info": map[string]interface{}{"llama.context_length": 131072}, "parameters": params})
	}))
	defer srv.Close()
	ec := EngineConfig{ID: 1, Name: "ollama", Type: EngineOllama, BaseURL: srv.URL}
	engineCachePtr.Store(&engineCache{engines: []EngineConfig{ec}, adapters: map[int]Engine{1: &OllamaAdapter{cfg: ec}}})
	probeCacheMu.Lock()
	probeCache[1] = probeEntry{expiry: time.Now().Add(time.Minute), probeData: probeData{status: true, raw: []map[string]interface{}{
		{"name": "ctx-modelfile", "modified_at": "2026-01-01T00:00:00Z"},
		{"name": "ctx-default", "modified_at": "2026-01-01T00:00:00Z"},
	}}}
	probeCacheMu.Unlock()

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer sk-gw-octx")
	setupRouter().ServeHTTP(rec, r)
	var list struct{ Data []map[string]interface{} }
	json.Unmarshal(rec.Body.Bytes(), &list)
	got := map[string]interface{}{}
	for _, m := range list.Data {
		got[m["id"].(string)] = m["max_input_tokens"]
	}
	// The Modelfile's num_ctx is the window Ollama uses; without one, it is the
	// server default the gateway cannot see, so nothing is claimed.
	if got["ctx-modelfile"] != 24576.0 || got["ctx-default"] != nil {
		t.Fatalf("max_input_tokens = %v\n%s", got, rec.Body.String())
	}
}

// ── Remote images ────────────────────────────────────────────────────────────

func TestIsPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "::1": false, "fd00::1": false,
		"fe80::1": false, "0.0.0.0": false, "224.0.0.1": false, "::ffff:127.0.0.1": false,
	} {
		if got := isPublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isPublicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The real fetcher refuses a loopback server — the SSRF guard — and the
// model is told the image could not be loaded.
func TestRemoteImageToPrivateAddressRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the gateway reached a loopback address")
	}))
	defer srv.Close()
	msgs := []map[string]interface{}{{"role": "user", "content": []interface{}{
		map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": srv.URL + "/secret.png"}},
	}}}
	out := inlineRemoteImages(context.Background(), msgs)
	part := out[0]["content"].([]interface{})[0].(map[string]interface{})
	if part["type"] != "text" || !strings.Contains(part["text"].(string), "could not be loaded") {
		t.Fatalf("part = %v", part)
	}
}

func TestRemoteImageInlinedAndCached(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/page.html" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>"))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("PNGDATA"))
	}))
	defer srv.Close()
	// The guard is covered above; here the client may reach the test server.
	prev := imageFetchClient
	imageFetchClient = srv.Client()
	t.Cleanup(func() { imageFetchClient = prev })

	img := func(path string) []map[string]interface{} {
		return []map[string]interface{}{{"role": "user", "content": []interface{}{
			map[string]interface{}{"type": "text", "text": "look"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": srv.URL + path}},
		}}}
	}
	in := img("/a.png")
	out := inlineRemoteImages(context.Background(), in)
	url := out[0]["content"].([]interface{})[1].(map[string]interface{})["image_url"].(map[string]interface{})["url"]
	if url != "data:image/png;base64,UE5HREFUQQ==" {
		t.Fatalf("url = %v", url)
	}
	if in[0]["content"].([]interface{})[1].(map[string]interface{})["image_url"].(map[string]interface{})["url"] == url {
		t.Error("the caller's messages were modified")
	}
	inlineRemoteImages(context.Background(), img("/a.png")) // next turn resends the image
	if hits != 1 {
		t.Errorf("fetched %d times, want 1 (cached)", hits)
	}
	// Not an image: refused, with a note.
	out = inlineRemoteImages(context.Background(), img("/page.html"))
	if p := out[0]["content"].([]interface{})[1].(map[string]interface{}); p["type"] != "text" {
		t.Errorf("non-image content inlined: %v", p)
	}
	// Turned off: left alone.
	prevFetch := cfg.FetchImageURLs
	cfg.FetchImageURLs = false
	t.Cleanup(func() { cfg.FetchImageURLs = prevFetch })
	if out := inlineRemoteImages(context.Background(), img("/b.png")); out[0]["content"].([]interface{})[1].(map[string]interface{})["type"] != "image_url" {
		t.Error("fetched with GW42_FETCH_IMAGE_URLS=false")
	}
}

func init() { cfg.FetchImageURLs = true }

// ── Timeout ──────────────────────────────────────────────────────────────────

func TestUpstreamTimeoutConfigurable(t *testing.T) {
	prev := cfg.UpstreamTimeoutSec
	t.Cleanup(func() { cfg.UpstreamTimeoutSec = prev })
	cfg.UpstreamTimeoutSec = 0
	if d := upstreamChatTimeout(); d != 600*time.Second {
		t.Errorf("default = %v, want 10m", d)
	}
	cfg.UpstreamTimeoutSec = 1800
	if d := upstreamChatTimeout(); d != 30*time.Minute {
		t.Errorf("1800 = %v", d)
	}
}

// ── PDFs in tool results ─────────────────────────────────────────────────────

func TestToolResultPDFBecomesFilePart(t *testing.T) {
	body := decodeJSON(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t1","content":[
			{"type":"document","title":"report","source":{"type":"base64","media_type":"application/pdf","data":"JVBE"}}]}]}]}`).(map[string]interface{})
	_, msgs := anthropicToOpenAI(body)
	got, _ := json.Marshal(msgs)
	for _, want := range []string{
		`{"content":"[The tool returned images or files; they follow in the next message.]","role":"tool","tool_call_id":"t1"}`,
		`{"file":{"file_data":"data:application/pdf;base64,JVBE","filename":"report.pdf"},"type":"file"}`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %s\n%s", want, got)
		}
	}
}

// ── Anthropic stream: pings, usage estimate, stop_sequence ───────────────────

// slowStream yields its payloads with a delay before each.
type slowStream struct {
	delay    time.Duration
	payloads []string
}

func (s *slowStream) Recv() ([]byte, chunkMeta, error) {
	if len(s.payloads) == 0 {
		return nil, chunkMeta{}, io.EOF
	}
	time.Sleep(s.delay)
	p := s.payloads[0]
	s.payloads = s.payloads[1:]
	return []byte(p), chunkMeta{}, nil
}
func (s *slowStream) Close() error { return nil }

func TestStreamPingsWhileEngineIsSilent(t *testing.T) {
	prev := pingInterval
	pingInterval = 20 * time.Millisecond
	t.Cleanup(func() { pingInterval = prev })

	eng := &fakeEngine{stream: &slowStream{delay: 120 * time.Millisecond, payloads: []string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop","stop_reason":"\n\nHuman:"}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`,
	}}}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	engineReq := map[string]interface{}{"messages": []interface{}{map[string]interface{}{"role": "user", "content": strings.Repeat("x", 400)}}}
	streamMessages(rec, r, eng, engineReq, 1, "m", nil, false)
	out := rec.Body.String()

	if n := strings.Count(out, "event: ping"); n < 3 {
		t.Errorf("pings = %d, want one after message_start and more while the engine was silent\n%s", n, out)
	}
	if i, j := strings.Index(out, "event: message_start"), strings.Index(out, "event: ping"); i < 0 || j < i {
		t.Errorf("first ping must follow message_start\n%s", out)
	}
	// message_start carries an input estimate; message_delta the engine's count.
	if !strings.Contains(out, `"usage":{"input_tokens":1`) {
		t.Errorf("message_start without an input estimate\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"stop_sequence","stop_sequence":"\n\nHuman:"`) {
		t.Errorf("stop_sequence not reported\n%s", out)
	}
	if !strings.Contains(out, `"input_tokens":9`) || !strings.Contains(out, "event: message_stop") {
		t.Errorf("final usage or message_stop missing\n%s", out)
	}
}

func TestNonStreamStopSequence(t *testing.T) {
	payload := decodeJSON(t, `{"choices":[{"index":0,"finish_reason":"stop","stop_reason":"END","message":{"role":"assistant","content":"x"}}]}`)
	msg, _ := openAIToAnthropic(payload, "m")
	if *msg.StopReason != "stop_sequence" || msg.StopSequence == nil || *msg.StopSequence != "END" {
		t.Fatalf("stop = %v %v", *msg.StopReason, msg.StopSequence)
	}
	// A token id (vLLM reports the EOS id this way) is not a stop sequence.
	payload = decodeJSON(t, `{"choices":[{"index":0,"finish_reason":"stop","stop_reason":151645,"message":{"role":"assistant","content":"x"}}]}`)
	msg, _ = openAIToAnthropic(payload, "m")
	if *msg.StopReason != "end_turn" || msg.StopSequence != nil {
		t.Fatalf("token-id stop = %v %v", *msg.StopReason, msg.StopSequence)
	}
}

// ── count_tokens ─────────────────────────────────────────────────────────────

func TestCountTokensFromEngine(t *testing.T) {
	var forwarded map[string]interface{}
	post := engineFixture(t, EngineOpenAICompat, "m", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&forwarded)
		w.Write([]byte(`{"input_tokens":1234}`))
	})
	rec := post("POST", "/v1/messages/count_tokens", "application/json",
		`{"model":"m","system":[{"type":"text","text":"x-anthropic-billing-header: cch=1;"},{"type":"text","text":"Be brief."}],"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"input_tokens":1234`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if forwarded["system"] != "Be brief." {
		t.Errorf("engine counted system %v, want the billing header stripped", forwarded["system"])
	}
}

func TestCountTokensFallsBackToEstimate(t *testing.T) {
	post := engineFixture(t, EngineOpenAICompat, "m", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	rec := post("POST", "/v1/messages/count_tokens", "application/json", `{"model":"m","messages":[{"role":"user","content":"hello there"}]}`)
	var got map[string]int
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["input_tokens"] <= 0 {
		t.Fatalf("got %d %s, want a positive estimate", rec.Code, rec.Body.String())
	}
}

// ── Pass-through endpoints ───────────────────────────────────────────────────

func TestPassthroughResponsesNonStreaming(t *testing.T) {
	var path, auth string
	var body map[string]interface{}
	post := engineFixture(t, EngineOpenAICompat, "m", func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":12,"output_tokens":3}}`))
	})
	drainRequestLog()
	rec := post("POST", "/v1/responses", "application/json", `{"model":"m","input":"hello","instructions":"be brief","tools":[{"type":"function","name":"f"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"resp_1"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if path != "/v1/responses" || auth != "" || body["instructions"] != "be brief" || body["tools"] == nil {
		t.Errorf("engine got %s auth=%q body=%v", path, auth, body)
	}
	rows := drainRequestLog()
	if len(rows) != 1 || rows[0].detail == nil || rows[0].detail.inTokens != 12 || rows[0].detail.outTokens != 3 {
		t.Errorf("request log usage = %+v", rows)
	}
}

func TestPassthroughResponsesStreaming(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":5}}}\n\n"
	post := engineFixture(t, EngineOpenAICompat, "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	})
	drainRequestLog()
	rec := post("POST", "/v1/responses", "application/json", `{"model":"m","input":"hello","stream":true}`)
	if rec.Code != 200 || rec.Body.String() != sse {
		t.Fatalf("stream not relayed byte for byte:\n%q\nwant\n%q", rec.Body.String(), sse)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	rows := drainRequestLog()
	if len(rows) != 1 || rows[0].detail.inTokens != 20 || rows[0].detail.outTokens != 5 || !rows[0].detail.streamed {
		t.Errorf("request log = %+v", rows[0].detail)
	}
}

func TestPassthroughCompletionsAsksForUsageOnStreams(t *testing.T) {
	var body map[string]interface{}
	post := engineFixture(t, EngineOllama, "m", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			t.Errorf("Ollama path = %s, want its OpenAI-compatible /v1/completions", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"text\":\"x\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	})
	rec := post("POST", "/v1/completions", "application/json", `{"model":"m","prompt":"Once","stream":true,"stream_options":{"foo":1}}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	so, _ := body["stream_options"].(map[string]interface{})
	if so["include_usage"] != true || so["foo"] != 1.0 {
		t.Errorf("stream_options = %v", so)
	}
}

func TestPassthroughAudioTranscriptionMultipart(t *testing.T) {
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	mw.WriteField("model", "whisper")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write([]byte("RIFFDATA"))
	mw.Close()

	var gotFile string
	var gotType string
	post := engineFixture(t, EngineOpenAICompat, "whisper", func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		if f, _, err := r.FormFile("file"); err == nil {
			b, _ := io.ReadAll(f)
			gotFile = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"hello"}`))
	})
	rec := post("POST", "/v1/audio/transcriptions", mw.FormDataContentType(), form.String())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"text":"hello"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if gotFile != "RIFFDATA" || !strings.HasPrefix(gotType, "multipart/form-data; boundary=") {
		t.Errorf("engine got file %q with type %q", gotFile, gotType)
	}
}

func TestPassthroughBinarySpeech(t *testing.T) {
	post := engineFixture(t, EngineOpenAICompat, "tts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte{0xFF, 0xFB, 0x90, 0x00})
	})
	rec := post("POST", "/v1/audio/speech", "application/json", `{"model":"tts","input":"hi","voice":"alloy"}`)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "audio/mpeg" || !bytes.Equal(rec.Body.Bytes(), []byte{0xFF, 0xFB, 0x90, 0x00}) {
		t.Fatalf("got %d %q %x", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes())
	}
}

func TestPassthroughErrors(t *testing.T) {
	post := engineFixture(t, EngineOpenAICompat, "m", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"detail":"Not Found"}`))
	})
	if rec := post("POST", "/v1/images/generations", "application/json", `{"model":"m","prompt":"a cat"}`); rec.Code != 404 || !strings.Contains(rec.Body.String(), "Not Found") {
		t.Errorf("unsupported endpoint: %d %s, want the engine's 404", rec.Code, rec.Body.String())
	}
	if rec := post("POST", "/v1/responses", "application/json", `{"input":"x"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "'model' is required") {
		t.Errorf("missing model: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("POST", "/v1/responses", "application/json", `{not json`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "Invalid JSON body") {
		t.Errorf("bad JSON: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPassthroughModerationsWithoutModelUsesDefaultEngine(t *testing.T) {
	post := engineFixture(t, EngineOpenAICompat, "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"flagged":false}]}`))
	})
	// model is optional for moderations: an empty one routes to the default
	// engine, the fixture's only one.
	rec := post("POST", "/v1/moderations", "application/json", `{"input":"hello"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"flagged":false`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestPassthroughUsageShapes(t *testing.T) {
	for body, want := range map[string][2]int{
		`{"usage":{"prompt_tokens":3,"completion_tokens":2}}`:                                     {3, 2},
		`{"usage":{"input_tokens":5,"output_tokens":7}}`:                                          {5, 7},
		`{"type":"response.completed","response":{"usage":{"input_tokens":9,"output_tokens":1}}}`: {9, 1},
	} {
		var m map[string]interface{}
		json.Unmarshal([]byte(body), &m)
		in, out, ok := passthroughUsage(m)
		if !ok || in != want[0] || out != want[1] {
			t.Errorf("%s → %d %d %v", body, in, out, ok)
		}
	}
}

// Non-streaming calls must not inherit the 30s header limit: an engine sends
// a non-streaming answer's headers only when the answer is complete.
func TestCompletionClientHasNoHeaderTimeout(t *testing.T) {
	if got := completionClient.Transport.(*http.Transport).ResponseHeaderTimeout; got != 0 {
		t.Errorf("completion client header timeout = %v, want none", got)
	}
	if sharedTransport.ResponseHeaderTimeout == 0 {
		t.Error("the shared (streaming) transport lost its header timeout")
	}
}

// End to end: an engine that holds its headers past a short shared limit
// still answers a non-streaming chat.
func TestSlowNonStreamingEngineNotCutByHeaderLimit(t *testing.T) {
	prev := sharedTransport.ResponseHeaderTimeout
	sharedTransport.ResponseHeaderTimeout = 50 * time.Millisecond
	t.Cleanup(func() { sharedTransport.ResponseHeaderTimeout = prev })
	post := engineFixture(t, EngineOpenAICompat, "m", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`))
	})
	rec := post("POST", "/v1/chat/completions", "application/json", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"done"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
