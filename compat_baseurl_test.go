package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenAI-compatible servers document their base URL with the /v1 on it —
// oMLX, LM Studio and vLLM all say "http://host:port/v1", and so do the
// OpenAI SDKs. The adapter appends /v1/... itself, so a base URL entered that
// way used to produce /v1/v1/models: a 404, an INACTIVE engine, and a 502 on
// every completion.

// newCompatUpstream serves only the real paths, so a doubled /v1 shows up as
// a 404 rather than being silently tolerated.
func newCompatUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[{"id":"qwen3-8b","object":"model"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAICompatBaseURLWithOrWithoutV1(t *testing.T) {
	srv := newCompatUpstream(t)
	for _, base := range []string{
		srv.URL,
		srv.URL + "/",
		srv.URL + "/v1",
		srv.URL + "/v1/",
	} {
		t.Run(base, func(t *testing.T) {
			a := &OpenAICompatAdapter{cfg: EngineConfig{Type: EngineOpenAICompat, BaseURL: base}}
			if up, err := a.Status(); !up || err != nil {
				t.Errorf("Status() = (%v, %v), want reachable", up, err)
			}
			models, err := a.ListModels()
			if err != nil || len(models) != 1 {
				t.Errorf("ListModels() = (%v, %v), want the one model", models, err)
			}
		})
	}
}

// A path prefix in front of /v1 (a reverse proxy mounting the engine under
// /openai, say) is part of the address and must survive.
func TestOpenAICompatBaseURLKeepsPathPrefix(t *testing.T) {
	a := &OpenAICompatAdapter{cfg: EngineConfig{BaseURL: "http://proxy/openai/v1"}}
	if got, want := a.upstream("/v1/chat/completions"), "http://proxy/openai/v1/chat/completions"; got != want {
		t.Errorf("upstream = %q, want %q", got, want)
	}
}

// An engine that answers with an error status is reachable but refusing, and
// the admin needs the status to tell a wrong API key (401) from a wrong path
// (404). "not responding" said neither.
func TestEngineTestReportsUpstreamStatus(t *testing.T) {
	withSessionStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	ec := EngineConfig{ID: 1, Name: "oMLX", Type: EngineOpenAICompat, BaseURL: srv.URL + "/v1"}
	prev := engineCachePtr.Load()
	engineCachePtr.Store(&engineCache{
		engines:  []EngineConfig{ec},
		adapters: map[int]Engine{1: &OpenAICompatAdapter{cfg: ec}},
	})
	t.Cleanup(func() { engineCachePtr.Store(prev) })

	rr := httptest.NewRecorder()
	handleEngineTest(rr, requestWithSession(t, http.MethodGet, "/admin/engine-test",
		map[interface{}]interface{}{"admin": true}))

	if f := flashesFrom(t, rr); !strings.Contains(f, "401") {
		t.Errorf("flash = %q, want the upstream 401 named", f)
	}
}

// Test Connectivity sits in the add form, so an admin who fills the form in
// and clicks it before Add Engine reasonably expects the typed values to be
// tested. They are not — only saved engines are — and the message has to say
// what to do instead of just "No engines configured".
func TestEngineTestWithNothingSavedSaysToAddFirst(t *testing.T) {
	withSessionStore(t)
	withEngines(t, nil)

	rr := httptest.NewRecorder()
	handleEngineTest(rr, requestWithSession(t, http.MethodGet, "/admin/engine-test",
		map[interface{}]interface{}{"admin": true}))

	if f := flashesFrom(t, rr); !strings.Contains(f, "Add Engine") {
		t.Errorf("flash = %q, want it to point the admin at Add Engine", f)
	}
}
