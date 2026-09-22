package main

// HTTP handlers for the admin web UI: session-cookie authenticated, HTML
// rendering, CSRF-protected form posts.
//
// The OpenAI-compatible /v1 surface lives in api_handlers.go. Anything added
// here should render a page or mutate admin state; anything that answers an SDK
// client belongs there.

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
)

// ─────────────────────────────── Index / login ────────────────────────────────

// handleIndex renders the login page
func handleIndex(w http.ResponseWriter, r *http.Request) {
	renderPage(w, "login", LoginData{BaseData: newBaseData(w, r)})
}

// handleLogout logs out the admin user
func handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	sess.Values["admin"] = false
	if !saveSession(w, r, sess) {
		// The browser still holds a cookie asserting admin. Redirecting to the
		// login page would tell the operator they are logged out when they are
		// not, which is the one outcome worth a 500 to avoid.
		http.Error(w, "could not end session", http.StatusInternalServerError)
		return
	}
	slog.Info("Admin logged out")
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleAdminLogin processes admin login requests
func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	password := r.FormValue("password")

	admin, err := getAdmin()
	if err != nil || admin == nil || !verifyPassword(admin.PasswordHash, password) {
		addFlash(w, r, "error", "Invalid password")
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	sess := getSession(r)
	sess.Values["admin"] = true
	sess.Options.MaxAge = cfg.SessionTTL
	if !saveSession(w, r, sess) {
		// Redirecting to the panel would bounce straight back to the login page,
		// since the cookie granting access was never issued. Say so instead.
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	slog.Info("Admin logged in")
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// ─────────────────────────────── Admin panel ──────────────────────────────────

// handleAdminPanel renders the admin panel page
func handleAdminPanel(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	users, err := getAllUsers()
	if err != nil {
		slog.Error("getAllUsers", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}

	engines := cachedEngines()
	enginesURLs := getEngineURLs(engines)
	enginesStatus := make(map[string]bool)
	enginesModels := make(map[string][]string)

	// Warm the probe cache concurrently with a hard 3s budget so a dead
	// engine cannot wedge the dashboard.
	warmCtx, warmCancel := context.WithTimeout(r.Context(), 3*time.Second)
	warmProbeCache(warmCtx)
	warmCancel()

	for _, e := range engines {
		adapter := cachedAdapter(e.ID)
		if adapter == nil {
			continue
		}
		pd := probeEngineCached(e.ID, adapter)
		enginesStatus[fmt.Sprintf("%d", e.ID)] = pd.status
		enginesModels[fmt.Sprintf("%d", e.ID)] = pd.models
	}

	renderPage(w, "dashboard", DashboardData{
		BaseData:     newBaseData(w, r),
		Users:        users,
		Engines:      engines,
		EngineURLs:   enginesURLs,
		EngineStatus: enginesStatus,
		EngineModels: enginesModels,
	})
}

// ─────────────────────────────── Settings page ────────────────────────────────

func handleAdminSettingsPage(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	engines := cachedEngines()
	engineURLs := getEngineURLs(engines)
	engineStatus := make(map[string]bool)
	engineModelDetails := make(map[string][]ModelDetail)

	warmCtx, warmCancel := context.WithTimeout(r.Context(), 3*time.Second)
	warmProbeCache(warmCtx)
	warmCancel()

	for _, e := range engines {
		adapter := cachedAdapter(e.ID)
		if adapter == nil {
			continue
		}
		pd := probeEngineCached(e.ID, adapter)
		engineStatus[fmt.Sprintf("%d", e.ID)] = pd.status
		engineModelDetails[fmt.Sprintf("%d", e.ID)] = pd.details
	}

	renderPage(w, "settings", SettingsData{
		BaseData:           newBaseData(w, r),
		Engines:            engines,
		EngineURLs:         engineURLs,
		EngineStatus:       engineStatus,
		EngineModelDetails: engineModelDetails,
		SearchResults:      []ModelDetail{}, // Will be populated by search
	})
}

func handleEngineTest(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	engines := cachedEngines()
	if len(engines) == 0 {
		addFlash(w, r, "error", "No engines configured")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	var results []string
	for _, e := range engines {
		adapter := cachedAdapter(e.ID)
		if adapter == nil {
			results = append(results, fmt.Sprintf("%s: error (cached adapter missing)", e.Name))
			continue
		}
		status, err := adapter.Status()
		if err != nil {
			results = append(results, fmt.Sprintf("%s: unreachable (%v)", e.Name, err))
			continue
		}
		if status {
			results = append(results, fmt.Sprintf("%s: OK (%s)", e.Name, e.Type))
		} else {
			results = append(results, fmt.Sprintf("%s: not responding", e.Name))
		}
	}
	msg := strings.Join(results, "; ")
	if len(results) == 1 && strings.Contains(results[0], "OK") {
		addFlash(w, r, "success", results[0])
	} else {
		addFlash(w, r, "info", msg)
	}
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

// ollamaPullStream streams a model pull from Ollama as SSE. Auth is checked by callers.
func ollamaPullStream(w http.ResponseWriter, r *http.Request, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	var adapter Engine
	if ollamas := cachedAdaptersByType(EngineOllama); len(ollamas) > 0 {
		adapter = ollamas[0]
	}
	if adapter == nil {
		fmt.Fprintf(w, "data: %s\n\n", jsonErr("No Ollama engine configured"))
		flusher.Flush()
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")

	endpoint := adapter.baseURL()
	body, _ := json.Marshal(map[string]interface{}{"name": model, "stream": true})

	ctx, cancel := context.WithTimeout(r.Context(), 600*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/api/pull", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(w, "data: %s\n\n", jsonErr(err.Error()))
		flusher.Flush()
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := sharedClient.Do(req)
	if err != nil {
		fmt.Fprintf(w, "data: %s\n\n", jsonErr(err.Error()))
		flusher.Flush()
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Fprintf(w, "data: %s\n\n", jsonErr(fmt.Sprintf("Ollama returned %d", resp.StatusCode)))
		flusher.Flush()
		return
	}

	scanner := newStreamScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func handleOllamaPullStream(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"Unauthorized"}`))
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"Model name required"}`))
		return
	}
	ollamaPullStream(w, r, model)
}

func jsonErr(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

func handleOllamaDeleteModel(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/settings-page") {
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		addFlash(w, r, "error", "Model name is required")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	// Target the engine the admin clicked on. Falling back to "first Ollama
	// engine" is only correct when there is exactly one; with several, a
	// missing engine_id would delete the model off the wrong host.
	wantID, _ := strconv.Atoi(r.FormValue("engine_id"))
	var target *EngineConfig
	for _, e := range cachedEngines() {
		if e.Type != EngineOllama {
			continue
		}
		if e.ID == wantID {
			target = &e
			break
		}
		if target == nil && wantID == 0 {
			target = &e
		}
	}
	if target == nil {
		addFlash(w, r, "error", "No Ollama engine available to delete from")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	port := target.Port
	if port == 0 {
		port = 11434
	}
	if err := OllamaDeleteModel(target.BaseURL, port, model); err != nil {
		slog.Error("delete model", "model", model, "engine", target.Name, "err", err)
		addFlash(w, r, "error", fmt.Sprintf("Could not delete '%s': %v", model, err))
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	// Drop the cached probe so the model list reflects the deletion now
	// rather than up to probeCacheTTL later.
	invalidateProbeCache()
	slog.Info("Model deleted", "model", model, "engine", target.Name)
	addFlash(w, r, "success", fmt.Sprintf("Model '%s' deleted from %s.", model, target.Name))

	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

func handleEngineSettings(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/settings-page") {
		return
	}

	engineType := r.FormValue("engine_type")
	engineName := strings.TrimSpace(r.FormValue("engine_name"))
	baseURL := strings.TrimSpace(r.FormValue("base_url"))
	portStr := strings.TrimSpace(r.FormValue("port"))
	apiKey := r.FormValue("api_key")
	editIDStr := r.FormValue("engine_id")

	// Validate type.
	if engineType != EngineOllama && engineType != EngineOpenAICompat {
		addFlash(w, r, "error", "Invalid engine type")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	// URL is required for all types.
	if baseURL == "" {
		addFlash(w, r, "error", "Base URL is required")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	// Port required for Ollama, default 11434.
	portInt := 11434
	if engineType == EngineOllama {
		if portStr == "" {
			addFlash(w, r, "error", "Port is required for Ollama engines")
			http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
			return
		}
		var err error
		portInt, err = strconv.Atoi(portStr)
		if err != nil || portInt < 1 || portInt > 65535 {
			addFlash(w, r, "error", "Port must be a number between 1 and 65535")
			http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
			return
		}
	}

	// Determine if editing existing or creating new.
	isEdit := editIDStr != ""
	var editID int
	if isEdit {
		var err error
		editID, err = strconv.Atoi(editIDStr)
		if err != nil {
			addFlash(w, r, "error", "Invalid engine ID")
			http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
			return
		}
	}

	engines, _ := getEngines()

	if isEdit {
		// Update existing engine.
		found := false
		for i := range engines {
			if engines[i].ID == editID {
				engines[i].Type = engineType
				engines[i].BaseURL = baseURL
				engines[i].APIKey = apiKey
				if engineName != "" {
					engines[i].Name = engineName
				}
				if engineType == EngineOllama {
					engines[i].Port = portInt
				}
				found = true
				break
			}
		}
		if !found {
			addFlash(w, r, "error", "Engine not found")
			http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
			return
		}
	} else {
		if engineName == "" {
			addFlash(w, r, "error", "Engine name is required")
			http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
			return
		}
		newID := 1
		for _, e := range engines {
			if e.ID >= newID {
				newID = e.ID + 1
			}
		}
		// Named ec, not cfg: cfg is the package-level Config global and
		// shadowing it here silently breaks every later cfg.* reference.
		ec := EngineConfig{
			ID:      newID,
			Name:    engineName,
			Type:    engineType,
			BaseURL: baseURL,
			APIKey:  apiKey,
		}
		if engineType == EngineOllama {
			ec.Port = portInt
		}
		engines = append(engines, ec)
	}

	if err := saveEngines(engines); err != nil {
		slog.Error("saveEngines", "err", err)
		addFlash(w, r, "error", "Failed to save engine config")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	action := "created"
	if isEdit {
		action = "updated"
	}
	slog.Info("Engine "+action, "id", editID, "type", engineType, "url", baseURL)
	addFlash(w, r, "success", "Engine "+action)
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

func handleRemoveEngine(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/settings-page") {
		return
	}

	editIDStr := r.FormValue("engine_id")
	if editIDStr == "" {
		addFlash(w, r, "error", "Engine ID required")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	editID, err := strconv.Atoi(editIDStr)
	if err != nil {
		addFlash(w, r, "error", "Invalid engine ID")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	engines, _ := getEngines()
	var filtered []EngineConfig
	for _, e := range engines {
		if e.ID != editID {
			filtered = append(filtered, e)
		}
	}

	if len(filtered) == len(engines) {
		addFlash(w, r, "error", "Engine not found")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	if err := saveEngines(filtered); err != nil {
		slog.Error("saveEngines", "err", err)
		addFlash(w, r, "error", "Failed to remove engine")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	slog.Info("Engine removed", "id", editID)
	addFlash(w, r, "success", "Engine removed")
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

// ─────────────────────────────── Model search ─────────────────────────────────

func handleOllamaSearch(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		jsonResponse(w, 401, map[string]string{"error": "Unauthorized"})
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		jsonResponse(w, 200, map[string][]ModelDetail{"results": []ModelDetail{}})
		return
	}

	results, err := OllamaSearchModels(query)
	if err != nil {
		slog.Warn("ollama.com search", "q", query, "err", err)
		jsonResponse(w, 502, map[string]string{"error": err.Error()})
		return
	}

	jsonResponse(w, 200, map[string][]ModelDetail{"results": results})
}

func handleOllamaPullSearchStream(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"Unauthorized"}`))
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"Model name required"}`))
		return
	}
	ollamaPullStream(w, r, model)
}

// ─────────────────────────────── Password change ──────────────────────────────

func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/settings-page") {
		return
	}
	current := r.FormValue("current")
	newPw := r.FormValue("new")
	confirm := r.FormValue("confirm")

	admin, err := getAdmin()
	if err != nil || admin == nil || !verifyPassword(admin.PasswordHash, current) {
		addFlash(w, r, "error", "Invalid current password")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	if newPw != confirm {
		addFlash(w, r, "error", "Passwords do not match")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	valid, msg := validatePassword(newPw)
	if !valid {
		addFlash(w, r, "error", msg)
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	if err := updateAdminPassword(admin.ID, hashPassword(newPw)); err != nil {
		slog.Error("change password", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	slog.Info("Admin password changed")
	// Invalidate session so the admin must re-authenticate with the new password.
	sess := getSession(r)
	sess.Values["admin"] = false
	b, _ := json.Marshal([]FlashMsg{{"success", "Password updated. Please log in again."}})
	sess.Values["flashes"] = string(b)
	// The password row is already updated and cannot be rolled back. A 500 here
	// would tell the admin the change failed when it succeeded, sending them
	// back to a password that no longer works — the worse of the two outcomes.
	//
	// The store is a CookieStore, so a failed save does leave this browser
	// holding admin=true until the cookie expires. That is the same principal
	// who just authenticated to change their own password, so the cost is a
	// skipped re-login, not an escalation.
	_ = saveSession(w, r, sess)
	http.Redirect(w, r, "/", http.StatusFound)
}

// ─────────────────────────────── User management ──────────────────────────────

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/panel") {
		return
	}
	name := truncateInput(r.FormValue("name"))

	if !validateName(name) {
		addFlash(w, r, "error", "Client name must be between 1 and 100 characters")
		http.Redirect(w, r, "/admin/panel", http.StatusFound)
		return
	}

	apiKey := generateAPIKey()
	if err := createUser(name, apiKey, cfg.DefaultRL); err != nil {
		addFlash(w, r, "error", err.Error())
		http.Redirect(w, r, "/admin/panel", http.StatusFound)
		return
	}

	slog.Info("User registered", "name", name)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w,
		`<h2 style='font-family:sans-serif'>Registration Successful</h2>`+
			`<p>API key for <strong>%s</strong>:</p>`+
			`<pre style='background:#111;color:#0f0;padding:12px'>%s</pre>`+
			`<p>Save this key — it will not be shown again.</p>`+
			`<p><a href='/admin/panel'>Back to admin panel</a></p>`,
		html.EscapeString(name), html.EscapeString(apiKey),
	)
}

func handleToggle(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	user, err := getUserByID(uid)
	if err != nil || user == nil {
		http.Error(w, "User not found", 404)
		return
	}
	newStatus := "active"
	if user.Status == "active" {
		newStatus = "disabled"
	}
	if err := updateUserStatus(uid, newStatus); err != nil {
		slog.Error("toggle user", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	slog.Info("User status toggled", "uid", uid, "status", newStatus)
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

func handleAdminResetKey(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	user, err := getUserByID(uid)
	if err != nil || user == nil {
		http.Error(w, "User not found", 404)
		return
	}
	newKey := generateAPIKey()
	if err := resetUserAPIKey(uid, newKey); err != nil {
		slog.Error("reset api key", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	slog.Info("API key reset", "uid", uid)
	addFlash(w, r, "success", fmt.Sprintf("New API key for %s: %s", user.Name, newKey))
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

func handleUpdateRateLimit(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	if !parseAdminForm(w, r, "/admin/panel") {
		return
	}
	rl, err := strconv.Atoi(r.FormValue("rate_limit"))
	if err != nil || rl < 1 {
		rl = 1
	}
	if rl > 1000 {
		rl = 1000
	}
	if err := updateUserRateLimit(uid, rl); err != nil {
		slog.Error("update rate limit", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	slog.Info("Rate limit updated", "uid", uid, "limit", rl)
	addFlash(w, r, "success", fmt.Sprintf("Rate limit updated for user %d: %d requests/minute", uid, rl))
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// ─────────────────────────────── Help ─────────────────────────────────────────

func handleAdminHelp(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	renderPage(w, "help", HelpData{BaseData: newBaseData(w, r)})
}

// ─────────────────────────────── Logs ─────────────────────────────────────────

func handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	renderPage(w, "logs", LogsData{
		BaseData: newBaseData(w, r),
		Search:   search,
	})
}

func handleAdminLogsData(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		jsonResponse(w, 401, map[string]string{"error": "Unauthorized"})
		return
	}
	const pageSize = 10
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	page := maxInt(1, queryInt(r, "page", 1))

	total, err := getRequestLogsCount(search)
	if err != nil {
		slog.Error("getRequestLogsCount", "err", err)
		jsonResponse(w, 500, map[string]string{"error": "Internal server error"})
		return
	}

	pages := maxInt(1, (total+pageSize-1)/pageSize)
	page = minInt(page, pages)
	offset := (page - 1) * pageSize

	slice, err := getRequestLogsPage(search, pageSize, offset)
	if err != nil {
		slog.Error("getRequestLogsPage", "err", err)
		jsonResponse(w, 500, map[string]string{"error": "Internal server error"})
		return
	}

	jsonResponse(w, 200, map[string]interface{}{
		"logs":   slice,
		"count":  len(slice),
		"total":  total,
		"page":   page,
		"pages":  pages,
		"search": search,
	})
}

func handleAdminSystemLogs(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		jsonResponse(w, 401, map[string]string{"error": "Unauthorized"})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	page := maxInt(1, queryInt(r, "page", 1))

	entries := syslogBuf.Entries()
	// Reverse (newest first)
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if q != "" {
		var filtered []SysLogEntry
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Msg), q) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	total := len(entries)
	pages := maxInt(1, (total+9)/10)
	page = minInt(page, pages)
	start := (page - 1) * 10
	end := minInt(start+10, total)
	var slice []SysLogEntry
	if start < total {
		slice = entries[start:end]
	}

	jsonResponse(w, 200, map[string]interface{}{
		"logs":   slice,
		"count":  len(slice),
		"total":  total,
		"page":   page,
		"pages":  pages,
		"search": q,
	})
}

func handleAdminSystemLogsExport(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	entries := syslogBuf.Entries()
	// Reverse
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if q != "" {
		var filtered []SysLogEntry
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Msg), q) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=system_logs.csv")
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	cw.Write([]string{"timestamp", "level", "logger", "message"})
	for _, e := range entries {
		cw.Write([]string{e.TS, e.Level, e.Name, e.Msg})
	}
	cw.Flush()
}

func handleAdminSystemLogsReset(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		jsonResponse(w, 401, map[string]string{"error": "Unauthorized"})
		return
	}
	syslogBuf.Clear()
	slog.Info("System log buffer cleared by admin")
	jsonResponse(w, 200, map[string]bool{"ok": true})
}

// ─────────────────────────────── CSV Export ───────────────────────────────────

func handleExport(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	rows, err := getUserLogs(uid)
	if err != nil {
		slog.Error("getUserLogs", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=user_%d_audit.csv", uid))
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	cw.Write([]string{"log_id", "prompt", "response", "timestamp"})
	for _, row := range rows {
		cw.Write([]string{strconv.Itoa(row.ID), row.Prompt, row.Response, row.TS})
	}
	cw.Flush()

	// Mark as exported in session
	sess := getSession(r)
	sess.Values[fmt.Sprintf("exported_%d", uid)] = true
	// The CSV is already written and flushed to the client. A lost flag only
	// means the UI will offer the export again.
	_ = saveSession(w, r, sess)
	slog.Info("CSV export", "uid", uid)
}

func handleExportAllLogs(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	sqlRows, err := dbRead.QueryContext(r.Context(),
		`SELECT id, ts, method, path, client_ip, user_name, status_code FROM request_logs ORDER BY ts DESC`)
	if err != nil {
		slog.Error("request_logs export", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	defer sqlRows.Close()

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=request_logs.csv")
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	cw.Write([]string{"id", "timestamp", "method", "path", "client_ip", "client_name", "status_code"})
	for sqlRows.Next() {
		var row RequestLogRow
		if err := sqlRows.Scan(&row.ID, &row.TS, &row.Method, &row.Path, &row.ClientIP, &row.UserName, &row.StatusCode); err != nil {
			// The 200 and the header row are already on the wire, so there is no
			// error status left to send. Stop at the first bad row: a truncated
			// prefix is recoverable by the operator, a silently skipped row in
			// the middle of an audit export is not.
			slog.Error("request_logs export scan", "err", err)
			break
		}
		cw.Write([]string{
			strconv.Itoa(row.ID), row.TS, row.Method, row.Path,
			row.ClientIP, row.UserName, strconv.Itoa(row.StatusCode),
		})
	}
	cw.Flush()
	if err := sqlRows.Err(); err != nil {
		slog.Error("request_logs export iteration", "err", err)
	}
	slog.Info("CSV export request logs")
}

// ─────────────────────────────── Delete user ──────────────────────────────────

func handleConfirmDelete(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	renderPage(w, "confirm_delete", ConfirmDeleteData{
		BaseData: newBaseData(w, r),
		UID:      uid,
	})
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	sess := getSession(r)

	exported, _ := sess.Values[fmt.Sprintf("exported_%d", uid)].(bool)
	if !exported {
		http.Error(w, "Export required before delete", http.StatusForbidden)
		return
	}

	if err := deleteUser(uid); err != nil {
		slog.Error("deleteUser", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	delete(sess.Values, fmt.Sprintf("exported_%d", uid))
	// The user row is already gone; the stale exported_ flag is cosmetic.
	_ = saveSession(w, r, sess)
	slog.Info("User deleted", "uid", uid)
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// ─────────────────────────────── Reset system ─────────────────────────────────

func handleResetSystem(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := resetSystem(); err != nil {
		slog.Error("resetSystem", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	slog.Info("System reset: all logs and rate-limit entries cleared")
	addFlash(w, r, "success", "System reset: all logs and rate-limit entries have been cleared.")
	// The reset already happened. This parse only feeds the redirect-target
	// read below, which is guarded by an allow-list, so a malformed body
	// falls through to the default target — the safe answer either way.
	_ = r.ParseForm()
	// Allow-list, not a free-form path: "next" is attacker-controllable, and
	// echoing it into a redirect unchecked is an open-redirect.
	dest := "/admin/panel"
	switch r.FormValue("next") {
	case "admin_logs":
		dest = "/admin/logs"
	case "settings_page":
		dest = "/admin/settings-page"
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// ─────────────────────────────── Users redirect ───────────────────────────────

func handleUsers(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// ─────────────────────────────── Helpers ──────────────────────────────────────

// parseAdminForm parses a browser form post, reporting failure the way every
// other validation failure in these handlers does: a flash plus a redirect back
// to the page that submitted it. Returns false when the caller should stop.
//
// The alternative — ignoring the error — is worse than it looks: a malformed
// body does not stop the handler, it reaches it as silently-empty form values.
// handleUpdateRateLimit, for one, would then read "" as the limit, fail the
// Atoi, and clamp the user to 1 request per window without saying why.
func parseAdminForm(w http.ResponseWriter, r *http.Request, back string) bool {
	if err := r.ParseForm(); err != nil {
		slog.Warn("malformed form submission", "path", r.URL.Path, "err", err)
		addFlash(w, r, "error", "Could not read the submitted form. Please try again.")
		http.Redirect(w, r, back, http.StatusFound)
		return false
	}
	return true
}

// saveSession persists a session and reports whether it stuck.
//
// The store writes through Set-Cookie, so a failure means the browser keeps the
// cookie it already had. Callers that are about to *announce* a state change —
// logged in, logged out — must not redirect on false: the redirect would assert
// a session the client never received. Callers whose change has already landed
// elsewhere (a password row, a deleted user) should carry on; the log line is
// the whole remedy available to them.
func saveSession(w http.ResponseWriter, r *http.Request, sess *sessions.Session) bool {
	if err := sess.Save(r, w); err != nil {
		slog.Error("session save failed", "path", r.URL.Path, "err", err)
		return false
	}
	return true
}

func uidFromVars(r *http.Request) int {
	vars := mux.Vars(r)
	uid, _ := strconv.Atoi(vars["uid"])
	return uid
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func jsonResponse(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
