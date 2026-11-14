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

// ── Request mapping ──────────────────────────────────────────────────────────

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
	streamMessages(rec, r, eng, engineReq, 1, "m", nil, false, nil, nil, false)
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
	post := engineFixture(t, EngineOMLX, "m", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			t.Errorf("engine path = %s, want /v1/completions", r.URL.Path)
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
