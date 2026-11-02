package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Test Connection checks the values typed into the engine form without saving
// them. It must build the config the way saving would — in particular, an edit
// with the API key left blank keeps the saved key, so the test has to use it.

// fakeOpenAI answers /v1/models with two models, but only for the given key.
func fakeOpenAI(t *testing.T, key string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testDraft(t *testing.T, form url.Values) (ok bool, msg string) {
	t.Helper()
	rr := httptest.NewRecorder()
	handleEngineTestDraft(rr, adminPost(t, "/admin/engine-test-draft", form))
	var out struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode reply: %v (body %q)", err, rr.Body.String())
	}
	return out.OK, out.Message
}

func TestEngineTestDraft(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	srv := fakeOpenAI(t, "sk-saved")
	if err := saveEngines([]EngineConfig{
		{ID: 1, Name: "omlx", Type: EngineOpenAICompat, BaseURL: srv.URL + "/v1", APIKey: "sk-saved"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tests := []struct {
		name   string
		form   url.Values
		wantOK bool
		want   string
	}{
		{"new engine, right key", url.Values{
			"engine_type": {EngineOpenAICompat}, "base_url": {srv.URL}, "api_key": {"sk-saved"},
		}, true, "Connected. 2 model(s) available."},
		{"new engine, wrong key", url.Values{
			"engine_type": {EngineOpenAICompat}, "base_url": {srv.URL}, "api_key": {"sk-wrong"},
		}, false, ""},
		{"edit, blank key keeps the saved one", url.Values{
			"engine_type": {EngineOpenAICompat}, "base_url": {srv.URL}, "engine_id": {"1"},
		}, true, "Connected. 2 model(s) available."},
		{"invalid form is rejected before any request", url.Values{
			"engine_type": {EngineOllama}, "base_url": {"http://127.0.0.1"}, "port": {"99999"},
		}, false, "Port must be a number between 1 and 65535"},
	}
	for _, tt := range tests {
		ok, msg := testDraft(t, tt.form)
		if ok != tt.wantOK || (tt.want != "" && msg != tt.want) {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", tt.name, ok, msg, tt.wantOK, tt.want)
		}
	}

	// Testing must never save: the list still holds only the seeded engine.
	if got := storedEngines(t); len(got) != 1 {
		t.Errorf("stored %d engines after testing, want 1", len(got))
	}
}
