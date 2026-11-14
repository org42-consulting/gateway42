package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── Thinking reaches the engine ──────────────────────────────────────────────

func TestThinkingSwitchSentToEngine(t *testing.T) {
	for body, want := range map[string]string{
		`{"model":"m","messages":[]}`:                                                    `"chat_template_kwargs":{"enable_thinking":false}`,
		`{"model":"m","thinking":{"type":"disabled"},"messages":[]}`:                     `"chat_template_kwargs":{"enable_thinking":false}`,
		`{"model":"m","thinking":{"type":"enabled","budget_tokens":4096},"messages":[]}`: `"chat_template_kwargs":{"enable_thinking":true}`,
		`{"model":"m","thinking":{"type":"adaptive"},"messages":[]}`:                     `"chat_template_kwargs":{"enable_thinking":true}`,
	} {
		data, raw := anthropicToOpenAI(decodeJSON(t, body).(map[string]interface{}))
		got, _ := json.Marshal(openAICompatRequest(data, sanitizeMessages(raw)))
		if !strings.Contains(string(got), want) {
			t.Errorf("%s → %s, want %s", body, got, want)
		}
	}
}

func TestEffortBecomesReasoningEffort(t *testing.T) {
	for effort, want := range map[string]string{"low": "low", "medium": "medium", "high": "high", "max": "high"} {
		body := `{"model":"m","output_config":{"effort":"` + effort + `"},"messages":[]}`
		data, raw := anthropicToOpenAI(decodeJSON(t, body).(map[string]interface{}))
		got, _ := json.Marshal(openAICompatRequest(data, sanitizeMessages(raw)))
		if !strings.Contains(string(got), `"reasoning_effort":"`+want+`"`) {
			t.Errorf("effort %s → %s, want reasoning_effort %s", effort, got, want)
		}
	}
	data, _ := anthropicToOpenAI(decodeJSON(t, `{"model":"m","messages":[]}`).(map[string]interface{}))
	if _, ok := data["reasoning_effort"]; ok {
		t.Error("reasoning_effort invented without an effort")
	}
}

func TestOllamaGetsThinkFalseOnly(t *testing.T) {
	off, _ := anthropicToOpenAI(decodeJSON(t, `{"model":"m","messages":[]}`).(map[string]interface{}))
	if req := openAIToOllama(off, nil); req["think"] != false {
		t.Errorf("thinking off: think = %v, want false", req["think"])
	}
	on, _ := anthropicToOpenAI(decodeJSON(t, `{"model":"m","thinking":{"type":"enabled"},"messages":[]}`).(map[string]interface{}))
	if req := openAIToOllama(on, nil); req["think"] != nil {
		t.Errorf("thinking on: think = %v, want unset (Ollama rejects true for non-thinking models)", req["think"])
	}
	if req := openAIToOllama(map[string]interface{}{"model": "m"}, nil); req["think"] != nil {
		t.Errorf("OpenAI request without kwargs: think = %v, want unset", req["think"])
	}
}

func TestOpenAIPlatformNeverGetsTemplateKwargs(t *testing.T) {
	data, raw := anthropicToOpenAI(decodeJSON(t, `{"model":"gpt-x","messages":[]}`).(map[string]interface{}))
	msgs := sanitizeMessages(raw)
	openai := &OpenAICompatAdapter{cfg: EngineConfig{Type: EngineOpenAICompat, BaseURL: "https://api.openai.com/v1"}}
	if req := buildEngineRequest(openai, data, msgs); req["chat_template_kwargs"] != nil {
		t.Errorf("api.openai.com got chat_template_kwargs: %v", req)
	}
	local := &OpenAICompatAdapter{cfg: EngineConfig{Type: EngineOpenAICompat, BaseURL: "http://127.0.0.1:8000/v1"}}
	if req := buildEngineRequest(local, data, msgs); req["chat_template_kwargs"] == nil {
		t.Errorf("local engine lost chat_template_kwargs: %v", req)
	}
}

func TestDisableParallelToolUse(t *testing.T) {
	data, _ := anthropicToOpenAI(decodeJSON(t, `{"model":"m","messages":[],
		"tools":[{"name":"t","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`).(map[string]interface{}))
	if data["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %v, want false", data["parallel_tool_calls"])
	}
}

// ── Context window in /v1/models ─────────────────────────────────────────────

func TestModelContextWindow(t *testing.T) {
	for _, c := range []struct {
		m    map[string]interface{}
		want int
	}{
		{map[string]interface{}{"max_model_len": 131072.0}, 131072},      // oMLX, vLLM
		{map[string]interface{}{"max_context_length": 32768.0}, 32768},   // LM Studio
		{map[string]interface{}{"context_length": 8192.0}, 8192},         // others
		{map[string]interface{}{"max_input_tokens": 1000000.0}, 1000000}, // Anthropic-shaped
		{map[string]interface{}{"size": 123.0}, 0},
	} {
		if got := modelContextWindow(c.m); got != c.want {
			t.Errorf("modelContextWindow(%v) = %d, want %d", c.m, got, c.want)
		}
	}
}

func TestListModelsCarriesContextWindowAndTier(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	resetProbeCache(t)
	foSeedUser(t, "ctx-user", "sk-gw-ctx", 60)
	ec := EngineConfig{ID: 1, Name: "omlx", Type: EngineOpenAICompat, BaseURL: "http://omlx"}
	engineCachePtr.Store(&engineCache{engines: []EngineConfig{ec}, adapters: map[int]Engine{1: &fakeEngine{}}})
	probeCacheMu.Lock()
	probeCache[1] = probeEntry{expiry: time.Now().Add(time.Minute), probeData: probeData{status: true, raw: []map[string]interface{}{
		{"id": "qwen3-coder", "created": 1.0, "max_model_len": 262144.0},
		{"id": "claude-sonnet-local", "created": 1.0, "anthropic_family_tier": "sonnet", "is_family_default": true},
		{"id": "plain", "created": 1.0},
	}}}
	probeCacheMu.Unlock()

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer sk-gw-ctx")
	setupRouter().ServeHTTP(rec, r)
	body := rec.Body.String()
	for _, want := range []string{
		`"id":"qwen3-coder"`, `"max_input_tokens":262144`,
		`"anthropic_family_tier":"sonnet"`, `"is_family_default":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/v1/models missing %s\n%s", want, body)
		}
	}
	// A model the engine says nothing about gets no invented window.
	var list struct{ Data []map[string]interface{} }
	json.Unmarshal(rec.Body.Bytes(), &list)
	for _, m := range list.Data {
		if m["id"] == "plain" {
			if _, ok := m["max_input_tokens"]; ok {
				t.Errorf("plain model got max_input_tokens: %v", m)
			}
		}
	}
}

// ── Embeddings ───────────────────────────────────────────────────────────────

// embedFixture routes model "emb" to one real adapter of engineType backed by
// handler, and returns a function that posts an embeddings body through the
// full router.
func embedFixture(t *testing.T, engineType string, handler http.HandlerFunc) func(body string) *httptest.ResponseRecorder {
	t.Helper()
	withSessionStore(t)
	withTestDB(t)
	foSeedUser(t, "emb-"+engineType, "sk-gw-emb-"+engineType, 600)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ec := EngineConfig{ID: 1, Name: "emb", Type: engineType, BaseURL: srv.URL}
	var a Engine = &OpenAICompatAdapter{cfg: ec, concurrencyLimiter: newConcurrencyLimiter(2)}
	if engineType == EngineOllama {
		a = &OllamaAdapter{cfg: ec, concurrencyLimiter: newConcurrencyLimiter(2)}
	}
	engineCachePtr.Store(&engineCache{engines: []EngineConfig{ec}, adapters: map[int]Engine{1: a}})
	storeRouteIndex(&routeIndex{byModel: map[string][]int{"emb": {1}}, built: time.Now(), probed: true}, routeIndexGeneration())
	t.Cleanup(invalidateRouteIndex)
	return func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sk-gw-emb-"+engineType)
		setupRouter().ServeHTTP(rec, r)
		return rec
	}
}

func TestEmbeddingsOpenAICompatPassthrough(t *testing.T) {
	var got map[string]interface{}
	var path string
	post := embedFixture(t, EngineOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.5,-1]}],"model":"emb","usage":{"prompt_tokens":3,"total_tokens":3}}`))
	})
	rec := post(`{"model":"emb","input":"hello","dimensions":2,"encoding_format":"float","junk":1}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"embedding":[0.5,-1]`) {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if path != "/v1/embeddings" {
		t.Errorf("engine path = %q", path)
	}
	if got["input"] != "hello" || got["dimensions"] != 2.0 || got["encoding_format"] != "float" || got["junk"] != nil {
		t.Errorf("forwarded body = %v", got)
	}
}

func TestEmbeddingsOllamaTranslated(t *testing.T) {
	var got map[string]interface{}
	var path string
	post := embedFixture(t, EngineOllama, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"model":"emb","embeddings":[[0.25,1],[2,-0.5]],"prompt_eval_count":7}`))
	})

	rec := post(`{"model":"emb","input":["a","b"]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if path != "/api/embed" || got["model"] != "emb" {
		t.Errorf("engine got %s %v", path, got)
	}
	var resp struct {
		Object string
		Data   []struct {
			Object    string
			Index     int
			Embedding []float64
		}
		Usage struct{ PromptTokens, TotalTokens int } `json:"usage"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Object != "list" || len(resp.Data) != 2 || resp.Data[1].Index != 1 ||
		resp.Data[1].Embedding[1] != -0.5 || resp.Data[0].Object != "embedding" {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"prompt_tokens":7`) {
		t.Errorf("usage missing: %s", rec.Body.String())
	}

	// base64: little-endian float32s, as OpenAI encodes them.
	rec = post(`{"model":"emb","input":"a","encoding_format":"base64"}`)
	var b64 struct{ Data []struct{ Embedding string } }
	json.Unmarshal(rec.Body.Bytes(), &b64)
	raw, err := base64.StdEncoding.DecodeString(b64.Data[0].Embedding)
	if err != nil || len(raw) != 8 || math.Float32frombits(binary.LittleEndian.Uint32(raw[4:])) != 1 {
		t.Fatalf("base64 embedding = %q (%v)", b64.Data[0].Embedding, err)
	}

	// Token input cannot be expressed to Ollama.
	if rec := post(`{"model":"emb","input":[1,2,3]}`); rec.Code != 400 {
		t.Errorf("token input on Ollama: status %d, want 400", rec.Code)
	}
}

func TestEmbeddingsValidationAndEngineErrors(t *testing.T) {
	post := embedFixture(t, EngineOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"model emb is not an embedding model"}}`))
	})
	if rec := post(`{"input":"x"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "'model' is required") {
		t.Errorf("missing model: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"model":"emb"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "'input' is required") {
		t.Errorf("missing input: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"model":"emb","input":"x"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "not an embedding model") {
		t.Errorf("engine rejection: %d %s", rec.Code, rec.Body.String())
	}
}
