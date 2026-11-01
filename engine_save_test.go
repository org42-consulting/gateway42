package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// The engine list is one JSON value in the settings table, so every edit is a
// read-modify-write. A read that fails must stop the edit: carrying on with an
// empty list and saving it replaces every configured engine with the one being
// added.

// withTestDB opens a fresh database in a temp directory for the duration of a
// test. main() normally does this; tests never run main.
func withTestDB(t *testing.T) {
	t.Helper()
	prevPath, prevDB, prevRead := cfg.DBPath, db, dbRead
	cfg.DBPath = filepath.Join(t.TempDir(), "gateway.db")
	if err := initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	t.Cleanup(func() {
		_ = dbRead.Close()
		_ = db.Close()
		cfg.DBPath, db, dbRead = prevPath, prevDB, prevRead
	})
	prevCache := engineCachePtr.Load()
	t.Cleanup(func() { engineCachePtr.Store(prevCache) })
}

// storedEngines reads the engine list through the writer pool, which stays
// usable after a test has broken the reader.
func storedEngines(t *testing.T) []EngineConfig {
	t.Helper()
	var val string
	if err := db.QueryRow("SELECT value FROM settings WHERE key='engines'").Scan(&val); err != nil {
		t.Fatalf("read engines: %v", err)
	}
	var out []EngineConfig
	if err := json.Unmarshal([]byte(val), &out); err != nil {
		t.Fatalf("decode engines: %v", err)
	}
	return out
}

// breakReads makes every getEngines call fail the way a transient read error
// would, while leaving writes working.
func breakReads(t *testing.T) {
	t.Helper()
	if err := dbRead.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
}

func adminPost(t *testing.T, target string, form url.Values) *http.Request {
	t.Helper()
	r := requestWithSession(t, http.MethodPost, target, map[interface{}]interface{}{"admin": true})
	r.Body = io.NopCloser(strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

var twoEngines = []EngineConfig{
	{ID: 1, Name: "gpu-a", Type: EngineOllama, BaseURL: "http://10.0.0.1", Port: 11434},
	{ID: 2, Name: "gpu-b", Type: EngineOllama, BaseURL: "http://10.0.0.2", Port: 11434},
}

func TestAddEngineDoesNotWipeListWhenReadFails(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	if err := saveEngines(twoEngines); err != nil {
		t.Fatalf("seed: %v", err)
	}
	breakReads(t)

	handleEngineSettings(httptest.NewRecorder(), adminPost(t, "/admin/engine-settings", url.Values{
		"engine_type": {EngineOllama}, "engine_name": {"gpu-c"},
		"base_url": {"http://10.0.0.3"}, "port": {"11434"},
	}))

	if got := storedEngines(t); len(got) != 2 {
		t.Fatalf("stored %d engines after a failed read, want the original 2: %+v", len(got), got)
	}
}

func TestAddEngineOnFreshDatabase(t *testing.T) {
	// No settings row yet is the normal first-run state, not a read failure.
	withSessionStore(t)
	withTestDB(t)

	handleEngineSettings(httptest.NewRecorder(), adminPost(t, "/admin/engine-settings", url.Values{
		"engine_type": {EngineOllama}, "engine_name": {"first"},
		"base_url": {"http://10.0.0.1"}, "port": {"11434"},
	}))

	got := storedEngines(t)
	if len(got) != 1 || got[0].Name != "first" || got[0].ID != 1 {
		t.Fatalf("stored %+v, want one engine named first with ID 1", got)
	}
}

func TestRemoveEngineReportsReadFailure(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	if err := saveEngines(twoEngines); err != nil {
		t.Fatalf("seed: %v", err)
	}
	breakReads(t)

	rr := httptest.NewRecorder()
	r := adminPost(t, "/admin/engine-remove", url.Values{"engine_id": {"2"}})
	handleRemoveEngine(rr, r)

	// The engine exists; saying "not found" would send the admin hunting for
	// a typo instead of a database problem.
	if flashes := flashesFrom(t, rr); strings.Contains(flashes, "not found") {
		t.Errorf("flash = %q; a read failure must not be reported as a missing engine", flashes)
	}
	if got := storedEngines(t); len(got) != 2 {
		t.Errorf("stored %d engines, want 2 untouched", len(got))
	}
}

// flashesFrom decodes the flash queue from the session cookie a handler set.
func flashesFrom(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rr.Result().Cookies() {
		r.AddCookie(c)
	}
	sess, err := sessionStore.Get(r, sessionName)
	if err != nil {
		t.Fatalf("session get: %v", err)
	}
	s, _ := sess.Values["flashes"].(string)
	return s
}
