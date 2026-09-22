package main

import (
	"strings"
	"testing"
)

// Renders the settings page against the same data shape
// handleAdminSettingsPage builds. html/template resolves fields at render
// time, not compile time, so without this a mistyped field is a 500 in the
// admin UI that `go build` and `go vet` both report as clean.
func TestRenderSettingsPage(t *testing.T) {
	initTemplates()

	tmpl := tmpls["settings"]
	if tmpl == nil {
		t.Fatal("settings template not registered")
	}

	data := SettingsData{
		BaseData: BaseData{CurrentPath: "/admin/settings-page"},
		Engines: []EngineConfig{
			{ID: 1, Name: "local-ollama", Type: EngineOllama, BaseURL: "http://127.0.0.1", Port: 11434},
			{ID: 2, Name: "lmstudio", Type: EngineOpenAICompat, BaseURL: "http://127.0.0.1:1234/v1"},
			{ID: 3, Name: "dead-engine", Type: EngineOllama, BaseURL: "http://127.0.0.1", Port: 9},
		},
		EngineURLs:   map[string]string{"1": "http://127.0.0.1:11434", "2": "http://127.0.0.1:1234/v1", "3": "http://127.0.0.1:9"},
		EngineStatus: map[string]bool{"1": true, "2": true, "3": false},
		EngineModelDetails: map[string][]ModelDetail{
			"1": {{Name: "llama3.2:latest", Size: "2.0 GB"}, {Name: "qwen3:8b", Size: ""}},
			"2": {{Name: "mlx-community/Qwen3-8B", Size: "N/A"}},
			"3": {},
		},
		SearchResults: []ModelDetail{},
	}

	var sb strings.Builder
	if err := tmpl.ExecuteTemplate(&sb, "base.html", data); err != nil {
		t.Fatalf("render settings: %v", err)
	}
	out := sb.String()

	// The per-model delete control must be bound to the right engine id and
	// must not appear for the non-Ollama engine.
	for _, want := range []string{
		`name="engine_id" value="1"`,
		`name="model" value="llama3.2:latest"`,
		`name="model" value="qwen3:8b"`,
		`<th>Actions</th>`,
		`name="next" value="settings_page"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered settings page missing %q", want)
		}
	}
	if strings.Contains(out, `name="model" value="mlx-community/Qwen3-8B"`) {
		t.Error("delete control rendered for a non-Ollama engine")
	}

	// Header/cell arity must match in the model-management table, or columns
	// silently shift (the Remove button used to sit under the "Models" header
	// with a fourth, unheadered cell after it). Scoped to the first table on
	// the page; the search-results table below it has its own headers.
	head, rows, _ := strings.Cut(out, "<tbody>")
	if got := strings.Count(head, "</th>"); got != 4 {
		t.Errorf("model-management table: got %d <th>, want 4", got)
	}
	firstRow, _, _ := strings.Cut(rows, "</tr>")
	if got := strings.Count(firstRow, "</td>"); got != 4 {
		t.Errorf("model-management row: got %d <td>, want 4", got)
	}

}

// initTemplates uses template.Must, so a parse error in any page — including
// base.html, which this change touched — panics here rather than at startup.
func TestAllPagesParse(t *testing.T) {
	initTemplates()
	for _, page := range []string{"dashboard", "settings", "logs", "help", "confirm_delete", "login"} {
		if tmpls[page] == nil {
			t.Errorf("%s: template not registered", page)
		}
	}
}
