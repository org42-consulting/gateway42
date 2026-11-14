package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOMLXAdmin is an oMLX engine's management API: model status, Hugging
// Face search, a download that progresses over a few polls, delete, load and
// unload. Every call must carry the engine's API key.
type fakeOMLXAdmin struct {
	t     *testing.T
	mu    sync.Mutex
	calls []string
	polls int
	srv   *httptest.Server
	fail  string // a download error to report, if any
}

func newFakeOMLXAdmin(t *testing.T) *fakeOMLXAdmin {
	f := &fakeOMLXAdmin{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOMLXAdmin) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer sk-omlx-main" {
		w.WriteHeader(401)
		w.Write([]byte(`{"detail":"Invalid API key"}`))
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/models/status":
		w.Write([]byte(`{"models":[{"id":"Qwen3-8B-4bit","loaded":true,"is_loading":false,"estimated_size":4600000000},
			{"id":"gemma-3-12b","loaded":false,"is_loading":true,"estimated_size":7000000000}]}`))
	case r.URL.Path == "/admin/api/hf/search":
		w.Write([]byte(`{"models":[{"repo_id":"mlx-community/Qwen3-8B-4bit","name":"mlx-community/Qwen3-8B-4bit","downloads":1234,"size":4600000000,"size_formatted":"4.6 GB"}],"total":1}`))
	case r.URL.Path == "/admin/api/hf/download" && r.Method == http.MethodPost:
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["repo_id"] != "mlx-community/Qwen3-8B-4bit" {
			w.WriteHeader(400)
			w.Write([]byte(`{"detail":"unknown repo"}`))
			return
		}
		w.Write([]byte(`{"success":true,"task":{"task_id":"t1","status":"pending"}}`))
	case r.URL.Path == "/admin/api/hf/tasks":
		f.mu.Lock()
		f.polls++
		n := f.polls
		f.mu.Unlock()
		status, done := "downloading", int64(n)*1000
		if n >= 3 {
			status, done = "completed", 3000
		}
		if f.fail != "" && n >= 2 {
			status = "failed"
		}
		b, _ := json.Marshal(map[string]interface{}{"tasks": []interface{}{
			map[string]interface{}{"task_id": "other", "status": "downloading", "total_size": 1, "downloaded_size": 0},
			map[string]interface{}{"task_id": "t1", "status": status, "total_size": 3000, "downloaded_size": done, "error": f.fail},
		}})
		w.Write(b)
	case strings.HasPrefix(r.URL.Path, "/admin/api/hf/models/") && r.Method == http.MethodDelete:
		w.Write([]byte(`{"success":true}`))
	case strings.HasSuffix(r.URL.Path, "/load") || strings.HasSuffix(r.URL.Path, "/unload"):
		w.Write([]byte(`{"status":"ok"}`))
	default:
		w.WriteHeader(404)
	}
}

// callLog is the management calls the engine received; the probe refresh a
// successful action triggers in the background (GET /v1/models) is left out.
func (f *fakeOMLXAdmin) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c != "GET /v1/models" {
			out = append(out, c)
		}
	}
	return out
}

// withOMLXEngine installs one oMLX engine (ID 1) backed by f, with key.
func withOMLXEngine(t *testing.T, f *fakeOMLXAdmin, key string) *OpenAICompatAdapter {
	t.Helper()
	ec := EngineConfig{ID: 1, Name: "mac", Type: EngineOMLX, BaseURL: f.srv.URL, APIKey: key}
	a := &OpenAICompatAdapter{cfg: ec, concurrencyLimiter: newConcurrencyLimiter(2)}
	prev := engineCachePtr.Load()
	engineCachePtr.Store(&engineCache{engines: []EngineConfig{ec}, adapters: map[int]Engine{1: a}})
	t.Cleanup(func() { engineCachePtr.Store(prev) })
	return a
}

func adminGet(t *testing.T, target string) *http.Request {
	t.Helper()
	return requestWithSession(t, http.MethodGet, target, map[interface{}]interface{}{"admin": true, csrfSessionKey: testCSRF})
}

func TestOMLXModelStatusOverlay(t *testing.T) {
	f := newFakeOMLXAdmin(t)
	a := withOMLXEngine(t, f, "sk-omlx-main")
	got := withOMLXStatus(t.Context(), a, []ModelDetail{{Name: "Qwen3-8B-4bit"}, {Name: "gemma-3-12b", Size: "7 GB"}, {Name: "unknown"}})
	if !got[0].Manageable || !got[0].Loaded || got[0].Size != "4.6 GB" {
		t.Errorf("loaded model = %+v", got[0])
	}
	if got[1].Loaded || !got[1].Loading || got[1].Size != "7 GB" {
		t.Errorf("loading model = %+v (listing size must win)", got[1])
	}
	if !got[2].Manageable || got[2].Loaded {
		t.Errorf("model without status = %+v", got[2])
	}
	// Status unavailable (wrong key): still manageable, nothing claimed.
	b := withOMLXEngine(t, f, "sk-wrong")
	got = withOMLXStatus(t.Context(), b, []ModelDetail{{Name: "Qwen3-8B-4bit"}})
	if !got[0].Manageable || got[0].Loaded {
		t.Errorf("without status = %+v", got[0])
	}
}

func TestOMLXSearch(t *testing.T) {
	withSessionStore(t)
	f := newFakeOMLXAdmin(t)
	withOMLXEngine(t, f, "sk-omlx-main")
	rec := httptest.NewRecorder()
	handleOMLXSearch(rec, adminGet(t, "/admin/omlx-search?q=qwen3+8b"))
	var body struct {
		Results []ModelDetail
		Error   string
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Results) != 1 || body.Results[0].Name != "mlx-community/Qwen3-8B-4bit" || body.Results[0].Size != "4.6 GB" || body.Results[0].Downloads != 1234 {
		t.Fatalf("search = %s", rec.Body.String())
	}
	if calls := f.callLog(); len(calls) != 1 || !strings.Contains(calls[0], "q=qwen3+8b") {
		t.Errorf("engine calls = %v", calls)
	}

	// A wrong key is explained, not passed on raw.
	withOMLXEngine(t, f, "sk-wrong")
	rec = httptest.NewRecorder()
	handleOMLXSearch(rec, adminGet(t, "/admin/omlx-search?q=x"))
	if !strings.Contains(rec.Body.String(), "check the engine's API key") {
		t.Errorf("401 answer = %s", rec.Body.String())
	}
	// Without an admin session, nothing.
	rec = httptest.NewRecorder()
	handleOMLXSearch(rec, httptest.NewRequest(http.MethodGet, "/admin/omlx-search?q=x", nil))
	if rec.Code != 401 {
		t.Errorf("unauthenticated search: %d", rec.Code)
	}
}

func TestOMLXDownloadStreamsProgress(t *testing.T) {
	withSessionStore(t)
	prev := omlxDownloadPoll
	omlxDownloadPoll = 5 * time.Millisecond
	t.Cleanup(func() { omlxDownloadPoll = prev })
	f := newFakeOMLXAdmin(t)
	withOMLXEngine(t, f, "sk-omlx-main")

	rec := httptest.NewRecorder()
	q := url.Values{"repo_id": {"mlx-community/Qwen3-8B-4bit"}, "_csrf": {testCSRF}}
	handleOMLXDownloadStream(rec, adminGet(t, "/admin/omlx-download-stream?"+q.Encode()))
	out := rec.Body.String()
	for _, want := range []string{
		`data: {"status":"Download started"}`,
		`data: {"completed":1000,"status":"downloading","total":3000}`,
		`data: {"completed":2000,"status":"downloading","total":3000}`,
		`data: {"completed":3000,"status":"success","total":3000}`,
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %s\n%s", want, out)
		}
	}
	if strings.Contains(out, `"task_id":"other"`) || strings.Count(out, `"total":1,`) > 0 {
		t.Errorf("another task's progress leaked into the stream:\n%s", out)
	}
	if calls := f.callLog(); calls[0] != "POST /admin/api/hf/download" {
		t.Errorf("first engine call = %v", calls)
	}
}

func TestOMLXDownloadFailures(t *testing.T) {
	withSessionStore(t)
	prev := omlxDownloadPoll
	omlxDownloadPoll = 5 * time.Millisecond
	t.Cleanup(func() { omlxDownloadPoll = prev })
	f := newFakeOMLXAdmin(t)
	f.fail = "disk full"
	withOMLXEngine(t, f, "sk-omlx-main")

	run := func(repo string) string {
		rec := httptest.NewRecorder()
		q := url.Values{"repo_id": {repo}, "_csrf": {testCSRF}}
		handleOMLXDownloadStream(rec, adminGet(t, "/admin/omlx-download-stream?"+q.Encode()))
		return rec.Body.String()
	}
	if out := run("mlx-community/Qwen3-8B-4bit"); !strings.Contains(out, `"error":"disk full"`) || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("failed task:\n%s", out)
	}
	if out := run("nobody/nothing"); !strings.Contains(out, `"error":"unknown repo"`) {
		t.Errorf("refused start:\n%s", out)
	}
}

func TestOMLXModelActions(t *testing.T) {
	withSessionStore(t)
	f := newFakeOMLXAdmin(t)
	withOMLXEngine(t, f, "sk-omlx-main")
	for _, c := range []struct{ action, call string }{
		{"load", "POST /v1/models/Qwen3-8B-4bit/load"},
		{"unload", "POST /v1/models/Qwen3-8B-4bit/unload"},
		{"delete", "DELETE /admin/api/hf/models/Qwen3-8B-4bit"},
	} {
		rec := httptest.NewRecorder()
		handleOMLXModelAction(rec, adminPost(t, "/admin/omlx-model-action", url.Values{
			"_csrf": {testCSRF}, "model": {"Qwen3-8B-4bit"}, "engine_id": {"1"}, "action": {c.action},
		}))
		if rec.Code != http.StatusFound {
			t.Errorf("%s: status %d", c.action, rec.Code)
		}
		calls := f.callLog()
		if len(calls) == 0 || calls[len(calls)-1] != c.call {
			t.Errorf("%s: engine got %v, want %s", c.action, calls, c.call)
		}
	}
	n := len(f.callLog())
	for _, form := range []url.Values{
		{"model": {"x"}, "engine_id": {"1"}, "action": {"format-disk"}},
		{"model": {"x"}, "engine_id": {"99"}, "action": {"delete"}},
		{"model": {""}, "engine_id": {"1"}, "action": {"delete"}},
	} {
		form.Set("_csrf", testCSRF)
		handleOMLXModelAction(httptest.NewRecorder(), adminPost(t, "/admin/omlx-model-action", form))
	}
	if got := len(f.callLog()); got != n {
		t.Errorf("invalid actions reached the engine: %v", f.callLog()[n:])
	}
}
