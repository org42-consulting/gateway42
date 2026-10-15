package main

import "testing"

// ollamaEndpoint is the single place an Ollama URL is assembled. Before it
// existed the adapter, the dashboard and model deletion each did it by hand,
// and only the adapter knew not to append a port that was already there.
func TestOllamaEndpoint(t *testing.T) {
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
			if got := ollamaEndpoint(tc.cfg); got != tc.want {
				t.Errorf("ollamaEndpoint(%+v) = %q, want %q", tc.cfg, got, tc.want)
			}
		})
	}
}

// The dashboard's endpoint column must agree with what the adapter dials.
func TestEngineEndpointMatchesAdapter(t *testing.T) {
	cf := EngineConfig{Type: EngineOllama, BaseURL: "http://10.0.0.5:11434", Port: 11434}
	if got, want := engineEndpoint(cf), (&OllamaAdapter{cfg: cf}).baseURL(); got != want {
		t.Errorf("dashboard shows %q, adapter dials %q", got, want)
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

func TestApplyEngineEditTypeSwitchClearsForeignFields(t *testing.T) {
	toOllama := EngineConfig{Type: EngineOpenAICompat, APIKey: "sk-secret"}
	applyEngineEdit(&toOllama, EngineOllama, "", "http://a", 11434, "")
	if toOllama.APIKey != "" {
		t.Errorf("switching to Ollama kept APIKey %q", toOllama.APIKey)
	}
	if toOllama.Port != 11434 {
		t.Errorf("Port = %d, want 11434", toOllama.Port)
	}

	toCompat := EngineConfig{Type: EngineOllama, Port: 11434}
	applyEngineEdit(&toCompat, EngineOpenAICompat, "", "http://a", 11434, "")
	if toCompat.Port != 0 {
		t.Errorf("switching to OpenAI-compatible kept Port %d", toCompat.Port)
	}
}

// withEngines installs a fixed engine cache for the duration of a test.
func withEngines(t *testing.T, engines []EngineConfig) {
	t.Helper()
	prev := engineCachePtr.Load()
	engineCachePtr.Store(&engineCache{engines: engines, adapters: map[int]Engine{}})
	t.Cleanup(func() { engineCachePtr.Store(prev) })
}

func TestFindOllamaEngine(t *testing.T) {
	withEngines(t, []EngineConfig{
		{ID: 1, Name: "compat", Type: EngineOpenAICompat},
		{ID: 2, Name: "gpu-a", Type: EngineOllama},
		{ID: 3, Name: "gpu-b", Type: EngineOllama},
	})

	if e := findOllamaEngine(3); e == nil || e.Name != "gpu-b" {
		t.Errorf("findOllamaEngine(3) = %v, want gpu-b", e)
	}
	if e := findOllamaEngine(0); e == nil || e.Name != "gpu-a" {
		t.Errorf("findOllamaEngine(0) = %v, want the first Ollama engine", e)
	}
	// An explicit choice that does not exist must not fall back: that is how
	// a pull or delete lands on the wrong host.
	if e := findOllamaEngine(99); e != nil {
		t.Errorf("findOllamaEngine(99) = %v, want nil", e)
	}
	// Nor may it resolve to a non-Ollama engine.
	if e := findOllamaEngine(1); e != nil {
		t.Errorf("findOllamaEngine(1) = %v, want nil for an OpenAI-compatible engine", e)
	}
}
