package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Token usage is derived from the request log rather than captured separately,
// so these tests cover both halves: the SQL aggregate that decides what the
// panel is allowed to claim, and the reshape into the nested rows the template
// walks.

func TestHumanNumKeepsThreeSignificantFigures(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1.00K"},
		{3100, "3.10K"},
		{12_345, "12.3K"},
		{88_200, "88.2K"},
		{100_300, "100K"},
		{402_110, "402K"},
		{1_284_500, "1.28M"},
		{12_340_000, "12.3M"},
		{123_400_000, "123M"},
	} {
		if got := humanNum(tc.in); got != tc.want {
			t.Errorf("humanNum(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUsageBarClassQuantizesAgainstTheLargestClient(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tokens, max int
		want        string
	}{
		{"largest", 1000, 1000, "ub-100"},
		{"exact third", 333, 1000, "ub-30"},
		{"floors to a visible sliver", 5, 1000, "ub-10"},
		{"zero gets no bar", 0, 1000, "ub-0"},
		// Every client spent zero: division by the (zero) maximum would be a
		// divide-by-zero panic with signed integers.
		{"empty set gets no bar", 0, 0, "ub-0"},
	} {
		if got := usageBarClass(tc.tokens, tc.max); got != tc.want {
			t.Errorf("%s: usageBarClass(%d, %d) = %q, want %q", tc.name, tc.tokens, tc.max, got, tc.want)
		}
	}
}

func TestUsageWindowForRejectsUnknownKeys(t *testing.T) {
	for _, key := range []string{"24h", "7d", "30d"} {
		if got := usageWindowFor(key); got != key {
			t.Errorf("usageWindowFor(%q) = %q, want it accepted unchanged", key, got)
		}
	}
	// The value arrives from a query string and reaches a SQL comparison, so
	// anything outside the allow-list must collapse to the default rather than
	// be parsed or passed through.
	for _, key := range []string{"", "7D", "week", "1=1' OR", "999d", "7"} {
		if got := usageWindowFor(key); got != usageWindowDefault {
			t.Errorf("usageWindowFor(%q) = %q, want the default %q", key, got, usageWindowDefault)
		}
	}
}

func TestBuildUsagePanelGroupsModelsUnderTheirClient(t *testing.T) {
	rows := []usageRow{
		{UserName: "big", Model: "qwen3:8b", Requests: 10, InTokens: 1000, OutTokens: 500,
			UserID: sql.NullInt64{Int64: 1, Valid: true}, Status: sql.NullString{String: "active", Valid: true}},
		{UserName: "big", Model: "llama3.2", Requests: 4, InTokens: 200, OutTokens: 100, Streamed: 2, Unreported: 1,
			UserID: sql.NullInt64{Int64: 1, Valid: true}, Status: sql.NullString{String: "active", Valid: true}},
		{UserName: "ghost", Model: "(unknown)", Requests: 3, InTokens: 0, OutTokens: 0, Unreported: 3},
	}

	p := buildUsagePanel(rows, "7d", 5)

	if p.Window != "7d" || p.WindowLabel != "7 days" {
		t.Errorf("window = %q/%q, want 7d/\"7 days\"", p.Window, p.WindowLabel)
	}
	if p.KnownClients != 5 {
		t.Errorf("KnownClients = %d, want 5", p.KnownClients)
	}
	if len(p.Clients) != 2 {
		t.Fatalf("got %d clients, want 2", len(p.Clients))
	}

	big := p.Clients[0]
	if big.Name != "big" {
		t.Errorf("ranked first = %q, want the client with the largest total", big.Name)
	}
	if big.Requests != 14 || big.InTokens != 1200 || big.OutTokens != 600 || big.Total() != 1800 {
		t.Errorf("big totals = %d req / %d in / %d out, want 14 / 1200 / 600 (total 1800)",
			big.Requests, big.InTokens, big.OutTokens)
	}
	if big.Streamed != 2 || big.Unreported != 1 {
		t.Errorf("big streamed/unreported = %d/%d, want 2/1", big.Streamed, big.Unreported)
	}
	if !big.Registered || big.Status != "active" {
		t.Errorf("big registered/status = %v/%q, want true/active", big.Registered, big.Status)
	}
	if len(big.Models) != 2 || big.Models[0].Model != "qwen3:8b" {
		t.Errorf("big models = %+v, want the query's two rows in order", big.Models)
	}
	// 1800 of 1800 is the whole scale.
	if big.Bar != "ub-100" {
		t.Errorf("big bar = %q, want ub-100", big.Bar)
	}

	ghost := p.Clients[1]
	if ghost.Registered {
		t.Error("an unjoined client name must not read as registered")
	}
	if ghost.Bar != "ub-0" {
		t.Errorf("ghost bar = %q, want ub-0 for a client that spent nothing", ghost.Bar)
	}

	if p.Totals.Clients != 2 || p.Totals.Requests != 17 || p.Totals.InTokens != 1200 ||
		p.Totals.OutTokens != 600 || p.Totals.Unreported != 4 {
		t.Errorf("totals = %+v, want 2 clients / 17 req / 1200 in / 600 out / 4 unreported", p.Totals)
	}
}

func TestBuildUsagePanelWithNoRows(t *testing.T) {
	p := buildUsagePanel(nil, "24h", 3)
	if len(p.Clients) != 0 {
		t.Errorf("got %d clients from no rows, want 0", len(p.Clients))
	}
	if p.Totals.Clients != 0 || p.Totals.Total() != 0 {
		t.Errorf("totals = %+v, want zeros", p.Totals)
	}
	if p.WindowLabel != "24 hours" {
		t.Errorf("WindowLabel = %q, want \"24 hours\"", p.WindowLabel)
	}
}

// seedRequestLog writes one request_logs row straight through the writer pool,
// bypassing the async queue so the read below is not racing a background flush.
func seedRequestLog(t *testing.T, ts, path, userName, modelServed, modelRequested string, in, out, streamed int) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO request_logs(ts, method, path, client_ip, user_name, status_code,
			model_served, model_requested, in_tokens, out_tokens, streamed)
		VALUES(?,?,?,?,?,?, ?,?,?,?,?)`,
		ts, "POST", path, "10.0.0.1", userName, 200,
		modelServed, modelRequested, in, out, streamed)
	if err != nil {
		t.Fatalf("seed request log: %v", err)
	}
}

func TestGetUsageByClient(t *testing.T) {
	withTestDB(t)

	if err := createUser("claude-code", "k1", 30); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if err := createUser("cursor", "k2", 10); err != nil {
		t.Fatalf("createUser: %v", err)
	}

	now := time.Now().UTC()
	recent := now.Add(-time.Hour).Format(time.RFC3339)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339)
	cutoff := now.Add(-24 * time.Hour).Format(time.RFC3339)

	// In window.
	seedRequestLog(t, recent, "/v1/chat/completions", "claude-code", "qwen3:8b", "qwen3:8b", 1000, 200, 1)
	seedRequestLog(t, recent, "/v1/messages", "claude-code", "qwen3:8b", "qwen3:8b", 500, 100, 0)
	// Same client, different model — must land in its own bucket.
	seedRequestLog(t, recent, "/v1/chat/completions", "claude-code", "llama3.2", "llama3.2", 70, 30, 0)
	// Requested but never resolved to a served model.
	seedRequestLog(t, recent, "/v1/chat/completions", "cursor", "", "mystery", 10, 5, 0)
	// Both counts zero: usage the engine never reported.
	seedRequestLog(t, recent, "/v1/chat/completions", "cursor", "mystery", "mystery", 0, 0, 1)

	// Excluded: outside the window.
	seedRequestLog(t, old, "/v1/chat/completions", "claude-code", "qwen3:8b", "qwen3:8b", 99_999, 99_999, 0)
	// Excluded: not a completion, and always zero tokens. Without the path
	// filter these inflate request counts and look like free usage.
	seedRequestLog(t, recent, "/v1/models", "claude-code", "", "", 0, 0, 0)
	seedRequestLog(t, recent, "/v1/messages/count_tokens", "claude-code", "", "", 0, 0, 0)
	// Excluded: a client deleted after the row was written still appears, but
	// unregistered.
	seedRequestLog(t, recent, "/v1/chat/completions", "legacy-bot", "qwen3:8b", "qwen3:8b", 0, 0, 0)

	rows, err := getUsageByClient(cutoff)
	if err != nil {
		t.Fatalf("getUsageByClient: %v", err)
	}

	type key struct{ user, model string }
	got := map[key]usageRow{}
	for _, r := range rows {
		got[key{r.UserName, r.Model}] = r
	}

	if _, ok := got[key{"claude-code", "qwen3:8b"}]; !ok {
		t.Fatal("missing the claude-code/qwen3:8b aggregate")
	}
	if cc := got[key{"claude-code", "qwen3:8b"}]; cc.Requests != 2 || cc.InTokens != 1500 ||
		cc.OutTokens != 300 || cc.Streamed != 1 || cc.Unreported != 0 {
		t.Errorf("claude-code/qwen3:8b = %+v, want 2 req / 1500 in / 300 out / 1 streamed / 0 unreported", cc)
	}
	// The 48h-old row's 99,999 tokens must not be in any bucket.
	for k, r := range got {
		if r.InTokens >= 99_999 {
			t.Errorf("bucket %v includes a row outside the window: %+v", k, r)
		}
	}

	if lm := got[key{"claude-code", "llama3.2"}]; lm.Requests != 1 || lm.InTokens != 70 {
		t.Errorf("claude-code/llama3.2 = %+v, want its own bucket of 1 req / 70 in", lm)
	}

	// Empty model_served falls back to model_requested.
	if m := got[key{"cursor", "mystery"}]; m.Requests != 2 || m.InTokens != 10 || m.OutTokens != 5 ||
		m.Unreported != 1 {
		t.Errorf("cursor/mystery = %+v, want 2 req / 10 in / 5 out / 1 unreported", m)
	}

	for _, r := range rows {
		if r.Model == "" {
			t.Error("a bucket kept an empty model name instead of the fallback")
		}
		if r.UserName == "ghost" {
			t.Error("ghost has no seeded rows")
		}
	}

	// The path filter: three /v1/models-or-count_tokens rows and one zero-token
	// completion. Request counts must exclude the former entirely.
	var modelsPathRepr bool
	for _, r := range rows {
		if r.UserName == "claude-code" && r.Model == "(unknown)" {
			modelsPathRepr = true
		}
	}
	if modelsPathRepr {
		t.Error("/v1/models rows were aggregated; the path filter is not holding")
	}

	// A deleted client's name survives the LEFT JOIN, unregistered.
	var legacy *usageRow
	for i := range rows {
		if rows[i].UserName == "legacy-bot" {
			legacy = &rows[i]
		}
	}
	if legacy == nil {
		t.Fatal("a deleted client's history vanished from the aggregate")
	}
	if legacy.UserID.Valid {
		t.Error("a deleted client joined a users row anyway")
	}
	if legacy.Unreported != 1 {
		t.Errorf("legacy-bot unreported = %d, want 1 for a zero-token row", legacy.Unreported)
	}

	// Registered clients do join, and carry their status for the panel.
	if cc := got[key{"claude-code", "qwen3:8b"}]; !cc.UserID.Valid || cc.Status.String != "disabled" {
		t.Errorf("claude-code join = %v/%q, want a valid id and the stored 'disabled' status",
			cc.UserID, cc.Status.String)
	}

	// Ranked by total tokens, largest first.
	if len(rows) < 2 {
		t.Fatalf("got %d buckets, want several to rank", len(rows))
	}
	prev := 1 << 62
	for _, r := range rows {
		if total := r.InTokens + r.OutTokens; total > prev {
			t.Errorf("buckets not ordered by descending tokens: %d after %d", total, prev)
		} else {
			prev = total
		}
	}
}

func TestUsagePanelRendersIntoTheDashboard(t *testing.T) {
	initTemplates()
	tmpl := tmpls["dashboard"]
	if tmpl == nil {
		t.Fatal("dashboard template not registered")
	}

	panel := buildUsagePanel([]usageRow{
		{UserName: "claude-code", Model: "qwen3-coder:30b", Requests: 142,
			InTokens: 1_001_200, OutTokens: 244_900, Streamed: 120},
		{UserName: "claude-code", Model: "llama3.2:latest", Requests: 33,
			InTokens: 283_300, OutTokens: 67_580, Unreported: 4},
		{UserName: "legacy-bot", Model: "(unknown)", Requests: 27, Unreported: 27},
	}, "7d", 7)

	var sb strings.Builder
	err := tmpl.ExecuteTemplate(&sb, "base.html", DashboardData{
		BaseData:     BaseData{CurrentPath: "/admin/panel", CSRFToken: "tok"},
		Engines:      []EngineConfig{{ID: 1, Name: "local", Type: EngineOllama, Port: 11434}},
		EngineURLs:   map[string]string{"1": "http://127.0.0.1:11434"},
		EngineStatus: map[string]bool{"1": true},
		EngineModels: map[string][]string{"1": {"qwen3-coder:30b"}},
		Users:        []User{{ID: 1, Name: "claude-code", Status: "active", RateLimit: 30}},
		Usage:        panel,
	})
	if err != nil {
		t.Fatalf("render dashboard with usage: %v", err)
	}
	out := sb.String()

	for _, want := range []string{
		`<h2>Token Usage</h2>`,
		`/admin/panel?usage=24h`,
		`class="page-btn is-current">7 days<`,
		`class="usage-client">claude-code<`,
		`ub-100`,
		// 1_001_200 + 244_900 + 283_300 + 67_580 across two model rows, and
		// the client total the panel ranks on.
		`1.25M`, `351K`, `1.60M`,
		`31 unreported`,
		`2 of 7 clients`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered dashboard missing %q", want)
		}
	}

	// A deleted client is labelled rather than silently dropped.
	if !strings.Contains(out, `DELETED`) {
		t.Error("an unregistered client name rendered without its DELETED badge")
	}
	// The bar width has to arrive as a quantized class. A style attribute is
	// how a computed percentage would get here, and static_css_test bans it.
	if strings.Contains(out, `style=`) {
		t.Error("a style attribute reached the rendered dashboard")
	}
}

func TestUsagePanelRendersWithoutUsage(t *testing.T) {
	initTemplates()
	var sb strings.Builder
	err := tmpls["dashboard"].ExecuteTemplate(&sb, "base.html", DashboardData{
		BaseData: BaseData{CurrentPath: "/admin/panel", CSRFToken: "tok"},
	})
	if err != nil {
		t.Fatalf("render dashboard with nil usage: %v", err)
	}
	if !strings.Contains(sb.String(), "Usage could not be read") {
		t.Error("a nil usage panel rendered no explanation")
	}
}

// Through the real router rather than the view model directly, because that is
// the only path that exercises the window parameter, the query, and the handler
// that ties them together.

func adminPanel(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := requestWithSession(t, http.MethodGet, target, map[interface{}]interface{}{"admin": true})
	rec := httptest.NewRecorder()
	setupRouter().ServeHTTP(rec, r)
	return rec
}

func TestAdminPanelShowsTokenUsage(t *testing.T) {
	withTestDB(t)
	withSessionStore(t)

	if err := createUser("claude-code", "k1", 30); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	stale := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	seedRequestLog(t, recent, "/v1/chat/completions", "claude-code", "qwen3:8b", "qwen3:8b", 10_000, 2_500, 1)
	seedRequestLog(t, stale, "/v1/chat/completions", "claude-code", "qwen3:8b", "qwen3:8b", 999_999, 999_999, 0)

	rec := adminPanel(t, "/admin/panel?usage=24h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %.500s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "<h2>Token Usage</h2>") {
		t.Error("the token usage panel did not render")
	}
	if !strings.Contains(body, `class="usage-client">claude-code<`) {
		t.Error("the active client is missing from the panel")
	}
	// 10_000 + 2_500 = 12_500 → 12.5K. The 48h row's million tokens must not
	// appear at this window.
	if !strings.Contains(body, "12.5K") {
		t.Errorf("the window's token total is missing; body has no 12.5K")
	}
	if strings.Contains(body, "2.00M") || strings.Contains(body, "1.99M") {
		t.Error("a request outside the 24h window was counted")
	}
	if !strings.Contains(body, `class="page-btn is-current">24 hours<`) {
		t.Error("the 24h control is not marked current")
	}
	// A client with no traffic in the window must still be listed by the
	// Registered Clients panel — the usage panel only shows who was active.
	if strings.Contains(body, "No chat requests") {
		t.Error("the panel reported no traffic despite a row inside the window")
	}
}

func TestAdminPanelUsageWindowFallsBackToTheDefault(t *testing.T) {
	withTestDB(t)
	withSessionStore(t)

	// An attacker-controlled query string that is not a known window must not
	// reach SQL as anything but the default's cutoff.
	rec := adminPanel(t, "/admin/panel?usage=1%27%20OR%20%271%27%3D%271")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Last 7 days of chat traffic") {
		t.Error("an unknown window did not fall back to the 7-day default")
	}
	if !strings.Contains(body, `class="page-btn is-current">7 days<`) {
		t.Error("the 7-day control is not marked current after the fallback")
	}
}

func TestAdminPanelUsageEmptyWindow(t *testing.T) {
	withTestDB(t)
	withSessionStore(t)

	if err := createUser("idle-client", "k1", 10); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	seedRequestLog(t, time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339),
		"/v1/chat/completions", "idle-client", "qwen3:8b", "qwen3:8b", 500, 100, 0)

	rec := adminPanel(t, "/admin/panel?usage=24h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No chat requests in the last 24 hours") {
		t.Error("a window with no completions did not say so")
	}
	// The empty state is about the window, not about the roster.
	if !strings.Contains(body, "idle-client") {
		t.Error("the Registered Clients panel lost a client that simply had no traffic")
	}
}
