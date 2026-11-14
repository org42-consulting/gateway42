package main

import "testing"

// legacyOllamaEndpoint is how the migration rebuilds an Ollama engine's URL,
// exactly as the removed Ollama adapter did.
func TestLegacyOllamaEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  EngineConfig
		want string
	}{
		{"port from config", EngineConfig{BaseURL: "http://10.0.0.5", Port: 11500}, "http://10.0.0.5:11500"},
		{"default port", EngineConfig{BaseURL: "http://10.0.0.5"}, "http://10.0.0.5:11434"},
		{"port already in URL is not doubled", EngineConfig{BaseURL: "http://10.0.0.5:11434", Port: 11434}, "http://10.0.0.5:11434"},
		{"URL port wins over config port", EngineConfig{BaseURL: "http://10.0.0.5:9999", Port: 11434}, "http://10.0.0.5:9999"},
		{"trailing slash", EngineConfig{BaseURL: "http://localhost/", Port: 11434}, "http://localhost:11434"},
		{"missing scheme", EngineConfig{BaseURL: "127.0.0.1", Port: 11434}, "http://127.0.0.1:11434"},
		{"https kept", EngineConfig{BaseURL: "https://ollama.example", Port: 443}, "https://ollama.example:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyOllamaEndpoint(tc.cfg); got != tc.want {
				t.Errorf("legacyOllamaEndpoint(%+v) = %q, want %q", tc.cfg, got, tc.want)
			}
		})
	}
}

// Engines saved with the removed Ollama type become OpenAI-compatible engines
// on Ollama's /v1 endpoint; others are left alone.
func TestMigrateLegacyEngines(t *testing.T) {
	withTestDB(t)
	if err := setSetting("engines", `[
		{"id":1,"name":"old","type":"ollama","base_url":"http://10.0.0.5","port":11500},
		{"id":2,"name":"omlx","type":"omlx","base_url":"http://127.0.0.1:8000","api_key":"k"}]`); err != nil {
		t.Fatal(err)
	}
	n, err := migrateLegacyEngines()
	if err != nil || n != 1 {
		t.Fatalf("migrated %d, %v; want 1", n, err)
	}
	engines, _ := getEngines()
	if e := engines[0]; e.Type != EngineOpenAICompat || e.BaseURL != "http://10.0.0.5:11500/v1" || e.Port != 0 {
		t.Errorf("migrated engine = %+v", e)
	}
	if e := engines[1]; e.Type != EngineOMLX || e.BaseURL != "http://127.0.0.1:8000" || e.APIKey != "k" {
		t.Errorf("oMLX engine changed: %+v", e)
	}
	if n, err := migrateLegacyEngines(); n != 0 || err != nil {
		t.Errorf("second run migrated %d, %v; want a no-op", n, err)
	}
	if _, err := newEngine(engines[0]); err != nil {
		t.Errorf("migrated engine does not load: %v", err)
	}
}

// The dashboard's endpoint column must agree with what the adapter dials.
func TestEngineEndpointMatchesAdapter(t *testing.T) {
	cf := EngineConfig{Type: EngineOMLX, BaseURL: "http://10.0.0.5:8000"}
	if got, want := engineEndpoint(cf), (&OpenAICompatAdapter{cfg: cf}).baseURL(); got != want {
		t.Errorf("dashboard shows %q, adapter dials %q", got, want)
	}
}

func TestOMLXEngineType(t *testing.T) {
	a := &OpenAICompatAdapter{cfg: EngineConfig{Type: EngineOMLX}}
	if a.Type() != EngineOMLX || !isOMLX(a) {
		t.Errorf("oMLX adapter type = %q", a.Type())
	}
	b := &OpenAICompatAdapter{cfg: EngineConfig{Type: EngineOpenAICompat}}
	if b.Type() != EngineOpenAICompat || isOMLX(b) {
		t.Errorf("compat adapter type = %q", b.Type())
	}
	if _, err := newEngine(EngineConfig{Type: "ollama"}); err == nil {
		t.Error("the removed ollama type must not load unmigrated")
	}
}

func TestApplyEngineEditKeepsAPIKeyWhenBlank(t *testing.T) {
	// The edit form never renders the stored key, so it always arrives blank
	// unless the admin is replacing it. Blank must mean "keep".
	e := EngineConfig{ID: 2, Name: "lmstudio", Type: EngineOpenAICompat, BaseURL: "http://a", APIKey: "sk-secret"}
	applyEngineEdit(&e, EngineOpenAICompat, "", "http://b", 0, "")

	if e.APIKey != "sk-secret" {
		t.Errorf("APIKey = %q, an unrelated edit must not strip the stored key", e.APIKey)
	}
	if e.Name != "lmstudio" {
		t.Errorf("Name = %q, a blank name must keep the old one", e.Name)
	}
	if e.BaseURL != "http://b" {
		t.Errorf("BaseURL = %q, want the edited value", e.BaseURL)
	}
}

func TestApplyEngineEditReplacesAPIKey(t *testing.T) {
	e := EngineConfig{Type: EngineOpenAICompat, APIKey: "old"}
	applyEngineEdit(&e, EngineOpenAICompat, "", "http://a", 0, "new")
	if e.APIKey != "new" {
		t.Errorf("APIKey = %q, want \"new\"", e.APIKey)
	}
}

func TestApplyEngineEditTypeSwitchKeepsKey(t *testing.T) {
	// Both engine types take an API key and carry the port in the base URL.
	e := EngineConfig{Type: EngineOpenAICompat, APIKey: "sk-secret", Port: 11434}
	applyEngineEdit(&e, EngineOMLX, "", "http://a:8000", 0, "")
	if e.Type != EngineOMLX || e.APIKey != "sk-secret" || e.Port != 0 {
		t.Errorf("after switching to oMLX: %+v", e)
	}
}

// withEngines installs a fixed engine cache for the duration of a test.
func withEngines(t *testing.T, engines []EngineConfig) {
	t.Helper()
	prev := engineCachePtr.Load()
	engineCachePtr.Store(&engineCache{engines: engines, adapters: map[int]Engine{}})
	t.Cleanup(func() { engineCachePtr.Store(prev) })
}

func TestOMLXEngineSelection(t *testing.T) {
	engines := []EngineConfig{
		{ID: 1, Name: "compat", Type: EngineOpenAICompat},
		{ID: 2, Name: "mac-a", Type: EngineOMLX},
		{ID: 3, Name: "mac-b", Type: EngineOMLX},
	}
	adapters := map[int]Engine{}
	for _, e := range engines {
		adapters[e.ID] = &OpenAICompatAdapter{cfg: e}
	}
	prev := engineCachePtr.Load()
	engineCachePtr.Store(&engineCache{engines: engines, adapters: adapters})
	t.Cleanup(func() { engineCachePtr.Store(prev) })

	if a := omlxEngine(3); a == nil || a.Name() != "mac-b" {
		t.Errorf("omlxEngine(3) = %v, want mac-b", a)
	}
	if a := omlxEngine(0); a == nil || a.Name() != "mac-a" {
		t.Errorf("omlxEngine(0) = %v, want the first oMLX engine", a)
	}
	// An explicit choice that does not exist must not fall back: that is how
	// a download or delete lands on the wrong host.
	if a := omlxEngine(99); a != nil {
		t.Errorf("omlxEngine(99) = %v, want nil", a)
	}
	// Nor may it resolve to a non-oMLX engine.
	if a := omlxEngine(1); a != nil {
		t.Errorf("omlxEngine(1) = %v, want nil for an OpenAI-compatible engine", a)
	}
}
