package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// ─────────────────────────────── CORS preflight ────────────────────────────────

// handleCorsPreflight handles CORS preflight requests
func handleCorsPreflight(w http.ResponseWriter, r *http.Request) {
	for k, v := range corsHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────────── Health ───────────────────────────────────────

// handleHealth returns a simple health check response
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// ─────────────────────────────── Index / login ────────────────────────────────

// handleIndex renders the login page
func handleIndex(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	flashes := consumeFlashes(w, r, sess)
	renderPage(w, "login", struct {
		Flashes []FlashMsg
	}{Flashes: flashes})
}

// handleLogout logs out the admin user
func handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	sess.Values["admin"] = false
	sess.Save(r, w)
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
	sess.Save(r, w)
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
	sess := getSession(r)
	flashes := consumeFlashes(w, r, sess)

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
		status, models, _ := probeEngineCached(e.ID, adapter)
		enginesStatus[fmt.Sprintf("%d", e.ID)] = status
		enginesModels[fmt.Sprintf("%d", e.ID)] = models
	}

	renderPage(w, "dashboard", DashboardData{
		BaseData:     BaseData{Flashes: flashes, CurrentPath: r.URL.Path},
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
	sess := getSession(r)
	flashes := consumeFlashes(w, r, sess)

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
		status, _, details := probeEngineCached(e.ID, adapter)
		engineStatus[fmt.Sprintf("%d", e.ID)] = status
		engineModelDetails[fmt.Sprintf("%d", e.ID)] = details
	}

	renderPage(w, "settings", SettingsData{
		BaseData:           BaseData{Flashes: flashes, CurrentPath: r.URL.Path},
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
	r.ParseForm()
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		addFlash(w, r, "error", "Model name is required")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	var baseURL string
	var port int
	for _, e := range cachedEngines() {
		if e.Type == EngineOllama {
			baseURL = e.BaseURL
			port = e.Port
			break
		}
	}
	if port == 0 {
		port = 11434
	}
	OllamaDeleteModel(baseURL, port, model)
	addFlash(w, r, "success", fmt.Sprintf("Model '%s' deleted.", model))

	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

func handleEngineSettings(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	r.ParseForm()

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
		cfg := EngineConfig{
			ID:       newID,
			Name:     engineName,
			Type:     engineType,
			BaseURL:  baseURL,
			APIKey:   apiKey,
		}
		if engineType == EngineOllama {
			cfg.Port = portInt
		}
		engines = append(engines, cfg)
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
	slog.Info("Engine " + action, "id", editID, "type", engineType, "url", baseURL)
	addFlash(w, r, "success", "Engine "+action)
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

func handleRemoveEngine(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	r.ParseForm()

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

	// Search ollama.com/search for the query
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("https://ollama.com/search?q=%s", url.QueryEscape(query)))
	if err != nil {
		jsonResponse(w, 502, map[string]string{"error": fmt.Sprintf("Could not reach ollama.com: %v", err)})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		jsonResponse(w, 502, map[string]string{"error": fmt.Sprintf("Ollama search returned %d", resp.StatusCode)})
		return
	}

	// Parse the HTML response to extract model names from /library/ links
	var results []ModelDetail
	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)

	// Look for links matching /library/model-name and /namespace/model-name patterns
	// e.g., <a href="/library/llama3.2" class="group w-full"> or
	//       <a href="/batiai/qwen3.6-35b" class="group w-full">
	re := regexp.MustCompile(`href="/(?:library/)?([^"/]+(?:/[^"/]+)?)\"[^>]*class="group w-full"`)
	matches := re.FindAllStringSubmatch(bodyStr, -1)

	// Use a map to deduplicate model names
	modelMap := make(map[string]bool)
	for _, m := range matches {
		if len(m) > 1 {
			modelName := m[1]
			if !modelMap[modelName] {
				modelMap[modelName] = true
				// Size is unknown for search results, show placeholder
				results = append(results, ModelDetail{Name: modelName, Size: "N/A"})
			}
		}
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
	r.ParseForm()
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
	sess.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// ─────────────────────────────── User management ──────────────────────────────

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	r.ParseForm()
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
		htmlEscape(name), htmlEscape(apiKey),
	)
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
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
	r.ParseForm()
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
	sess := getSession(r)
	flashes := consumeFlashes(w, r, sess)
	renderPage(w, "help", HelpData{BaseData: BaseData{Flashes: flashes, CurrentPath: r.URL.Path}})
}

// ─────────────────────────────── Logs ─────────────────────────────────────────

func handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	sess := getSession(r)
	flashes := consumeFlashes(w, r, sess)
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	renderPage(w, "logs", LogsData{
		BaseData: BaseData{Flashes: flashes, CurrentPath: r.URL.Path},
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
	sess.Save(r, w)
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
		sqlRows.Scan(&row.ID, &row.TS, &row.Method, &row.Path, &row.ClientIP, &row.UserName, &row.StatusCode)
		cw.Write([]string{
			strconv.Itoa(row.ID), row.TS, row.Method, row.Path,
			row.ClientIP, row.UserName, strconv.Itoa(row.StatusCode),
		})
	}
	cw.Flush()
	slog.Info("CSV export request logs")
}

// ─────────────────────────────── Delete user ──────────────────────────────────

func handleConfirmDelete(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	sess := getSession(r)
	flashes := consumeFlashes(w, r, sess)
	renderPage(w, "confirm_delete", ConfirmDeleteData{
		BaseData: BaseData{Flashes: flashes, CurrentPath: r.URL.Path},
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
	sess.Save(r, w)
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
	r.ParseForm()
	next := r.FormValue("next")
	if next != "admin_panel" && next != "admin_logs" {
		next = "admin_panel"
	}
	dest := "/admin/panel"
	if next == "admin_logs" {
		dest = "/admin/logs"
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

// ─────────────────────────────── API: models ──────────────────────────────────

func handleListModels(w http.ResponseWriter, r *http.Request) {
	// Auth already resolved by apiAuthMiddleware; if user is nil here, the
	// middleware was bypassed (programming error).
	if userFromContext(r) == nil {
		jsonResponse(w, 401, openaiError("Invalid API key", "authentication_error"))
		return
	}

	engines := cachedEngines()
	if len(engines) == 0 {
		jsonResponse(w, 502, openaiError("No engines configured", "api_error"))
		return
	}
	adapter := cachedAdapter(engines[0].ID)
	if adapter == nil {
		jsonResponse(w, 502, openaiError("No engine configured", "api_error"))
		return
	}

	models, err := adapter.ListModels()
	if err != nil {
		slog.Error("engine ListModels", "err", err)
		jsonResponse(w, 502, openaiError("Could not reach engine", "api_error"))
		return
	}

	// Convert engine models to OpenAI format
	modelList := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		name := modelName(m)
		if name == "" {
			continue
		}
		sizeB := toInt(m["size"])
		modelList = append(modelList, map[string]interface{}{
			"id":       name,
			"object":   "model",
			"created":  int64(0),
			"owned_by": engines[0].Type,
			"size":     sizeB,
		})
	}

	jsonResponse(w, 200, map[string]interface{}{
		"object": "list",
		"data":   modelList,
	})
}

// ─────────────────────────────── API: chat completions ────────────────────────

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		jsonResponse(w, 401, openaiError("Invalid API key", "authentication_error"))
		return
	}

	if !isAllowed(user.ID, user.RateLimit) {
		metricRateLimited.Inc()
		jsonResponse(w, 429, openaiError("Rate limit exceeded", "rate_limit_error"))
		return
	}

	// Bound the request body. Allow ~8× MaxMsgLen so a multi-turn conversation
	// of bounded-length messages still fits; truncateInput then trims each field.
	r.Body = http.MaxBytesReader(w, r.Body, int64(cfg.MaxMsgLen)*8)

	var data map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil || data == nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonResponse(w, 413, openaiError("Request body too large", "invalid_request_error"))
			return
		}
		jsonResponse(w, 400, openaiError("Invalid JSON body", "invalid_request_error"))
		return
	}
	if _, ok := data["messages"]; !ok {
		jsonResponse(w, 400, openaiError("'messages' is required", "invalid_request_error"))
		return
	}

	rawMsgs, _ := data["messages"].([]interface{})
	messages := sanitizeMessages(rawMsgs)
	model, _ := data["model"].(string)

	// Use the first Ollama engine, or fall back to the first configured engine.
	var adapter Engine
	if ollamas := cachedAdaptersByType(EngineOllama); len(ollamas) > 0 {
		adapter = ollamas[0]
	} else if engines := cachedEngines(); len(engines) > 0 {
		adapter = cachedAdapter(engines[0].ID)
	}
	if adapter == nil {
		jsonResponse(w, 502, openaiError("No engines configured", "api_error"))
		return
	}

	// Cap upstream concurrency. Wait up to 250ms for a slot before giving
	// up — long enough to absorb a momentary spike, short enough that
	// clients see a clear backpressure signal.
	acqCtx, acqCancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
	gotSlot := adapter.TryAcquire(acqCtx)
	acqCancel()
	if !gotSlot {
		metricUpstreamBusy.WithLabelValues(adapter.Type()).Inc()
		w.Header().Set("Retry-After", "2")
		jsonResponse(w, 503, openaiError("Engine busy, please retry", "api_error"))
		return
	}
	defer adapter.Release()

	// Build the request for the engine — translate only for Ollama adapters.
	var engineReq map[string]interface{}
	if adapter.Type() == EngineOllama {
		engineReq = openAIToOllama(data, messages)
	} else {
		engineReq = map[string]interface{}{
			"model":    data["model"],
			"messages": messages,
			"stream":   data["stream"],
		}
		// pass through recognized OpenAI params
		for key := range data {
			switch key {
			case "model", "messages", "stream", "temperature", "top_p", "seed", "max_tokens",
				"max_completion_tokens", "frequency_penalty", "presence_penalty", "stop",
				"provider_options", "response_format", "service_tier", "logit_bias", "tools",
				"tool_choice", "logprobs", "top_logprobs", "parallel_tool_calls",
				"user", "n":
				engineReq[key] = data[key]
			}
		}
	}

	streaming, _ := data["stream"].(bool)
	if streaming {
		wrapper, ok := adapter.(*OllamaAdapter)
		if ok {
			wrapper.ChatStream(w, r, engineReq, user.ID, model)
			return
		}
		// Fallback: use the adapter's ChatStream method
		adapter.ChatStream(w, r, engineReq, user.ID, model)
		return
	}

	// Non-streaming
	result, err := adapter.Chat(engineReq)
	if err != nil {
		slog.Error("engine request", "err", err)
		jsonResponse(w, 502, openaiError("Could not reach engine", "api_error"))
		return
	}

	logInteraction(user.ID, marshalAudit(messages), marshalAudit(result), model)
	jsonResponse(w, 200, result)
}


// getAuthenticatedUser is retained for any caller that still resolves a
// /v1/* user outside the middleware. Prefer userFromContext when running
// inside the middleware-protected request pipeline.
func getAuthenticatedUser(r *http.Request) (*User, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil, nil
	}
	user, err := getUserByAPIKey(auth[7:])
	if err != nil {
		return nil, err
	}
	if user == nil || user.Status != "active" {
		return nil, nil
	}
	return user, nil
}

func sanitizeMessages(messages []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)
		out = append(out, map[string]interface{}{
			"role":    role,
			"content": truncateInput(content),
		})
	}
	return out
}

// ─────────────────────────────── Helpers ──────────────────────────────────────

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
