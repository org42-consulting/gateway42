package main

// Migration of engines saved with the removed Ollama engine type.
//
// Gateway42 used to speak Ollama's native API (/api/chat, /api/tags) through
// its own adapter. That adapter was replaced by oMLX support. An engine still
// configured as "ollama" would no longer load, so at startup it is rewritten
// as an OpenAI-compatible engine on Ollama's own OpenAI-compatible endpoint,
// <host>:<port>/v1, which serves chat completions, embeddings and models.
// Requests keep working; only the Ollama-specific admin features (ollama.com
// search, pull, delete) are gone, as are options only the native API took.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
)

const legacyEngineOllama = "ollama"

// legacyOllamaEndpoint assembles an Ollama engine's scheme://host:port the
// way the old adapter did: a scheme added when missing, and port 11434 when
// neither the base URL nor the Port field gives one.
func legacyOllamaEndpoint(cf EngineConfig) string {
	u := strings.TrimRight(strings.TrimSpace(cf.BaseURL), "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	hostPort := strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
	if _, _, err := net.SplitHostPort(hostPort); err == nil {
		return u
	}
	port := cf.Port
	if port == 0 {
		port = 11434
	}
	return fmt.Sprintf("%s:%d", u, port)
}

// migrateLegacyEngines rewrites saved "ollama" engines as OpenAI-compatible
// ones and reports how many it changed. Run once at startup, before the
// engine cache loads; a no-op when there are none.
func migrateLegacyEngines() (int, error) {
	engines, err := getEngines()
	if err != nil {
		return 0, err
	}
	changed := 0
	for i, e := range engines {
		if e.Type != legacyEngineOllama {
			continue
		}
		engines[i].Type = EngineOpenAICompat
		engines[i].BaseURL = legacyOllamaEndpoint(e) + "/v1"
		engines[i].Port = 0
		changed++
		slog.Warn("engine migrated: Ollama support was replaced by oMLX; this engine now uses Ollama's OpenAI-compatible API",
			"engine", e.Name, "base_url", engines[i].BaseURL)
	}
	if changed == 0 {
		return 0, nil
	}
	b, err := json.Marshal(engines)
	if err != nil {
		return 0, err
	}
	return changed, setSetting("engines", string(b))
}
