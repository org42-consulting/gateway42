package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
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

	ollamaURL, ollamaPort := parseOllamaSettings()
	ollamaStatus, ollamaModels := probeOllama(ollamaURL, ollamaPort)

	renderPage(w, "dashboard", DashboardData{
		BaseData:     BaseData{Flashes: flashes, CurrentPath: r.URL.Path},
		Users:        users,
		OllamaURL:    ollamaURL,
		OllamaPort:   ollamaPort,
		OllamaStatus: ollamaStatus,
		OllamaModels: ollamaModels,
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

	ollamaURL, ollamaPort := parseOllamaSettings()
	ollamaStatus, ollamaModels := probeOllama(ollamaURL, ollamaPort)
	var modelDetails []ModelDetail
	if ollamaStatus {
		modelDetails = fetchModelDetails(ollamaURL, ollamaPort)
	}

	renderPage(w, "settings", SettingsData{
		BaseData:           BaseData{Flashes: flashes, CurrentPath: r.URL.Path},
		OllamaURL:          ollamaURL,
		OllamaPort:         ollamaPort,
		OllamaStatus:       ollamaStatus,
		OllamaModels:       ollamaModels,
		OllamaModelDetails: modelDetails,
		SearchResults:      []ModelDetail{}, // Will be populated by search
	})
}

// probeOllama checks connectivity and returns (status, modelNames).
func probeOllama(baseURL string, port int) (bool, []string) {
	endpoint := fmt.Sprintf("%s:%d", baseURL, port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(endpoint + "/api/version")
	if err != nil || resp.StatusCode != 200 {
		return false, nil
	}
	resp.Body.Close()

	client2 := &http.Client{Timeout: 5 * time.Second}
	resp2, err := client2.Get(endpoint + "/api/tags")
	if err != nil || resp2.StatusCode != 200 {
		return true, nil
	}
	defer resp2.Body.Close()
	var tags map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&tags)
	models, _ := tags["models"].([]interface{})
	var names []string
	for _, m := range models {
		if mm, ok := m.(map[string]interface{}); ok {
			if name, ok := mm["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return true, names
}

func fetchModelDetails(baseURL string, port int) []ModelDetail {
	endpoint := fmt.Sprintf("%s:%d", baseURL, port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(endpoint + "/api/tags")
	if err != nil || resp.StatusCode != 200 {
		return nil
	}
	defer resp.Body.Close()
	var tags map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tags)
	models, _ := tags["models"].([]interface{})
	var out []ModelDetail
	for _, m := range models {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := mm["name"].(string)
		sizeB := toInt(mm["size"])
		var sizeStr string
		if sizeB >= 1_000_000_000 {
			sizeStr = fmt.Sprintf("%.1f GB", float64(sizeB)/1e9)
		} else {
			sizeStr = fmt.Sprintf("%d MB", sizeB/1_000_000)
		}
		out = append(out, ModelDetail{Name: name, Size: sizeStr})
	}
	return out
}

// ─────────────────────────────── Ollama management ────────────────────────────

func handleOllamaTest(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	baseURL := getOllamaBaseURL()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + "/api/version")
	if err != nil {
		addFlash(w, r, "error", fmt.Sprintf("Could not reach Ollama: %v", err))
	} else {
		defer resp.Body.Close()
		var ver map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&ver)
		version, _ := ver["version"].(string)
		if version == "" {
			version = "unknown"
		}
		addFlash(w, r, "success", fmt.Sprintf("Connected — Ollama version %s", version))
	}
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
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

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")

	baseURL := getOllamaBaseURL()
	body, _ := json.Marshal(map[string]interface{}{"name": model, "stream": true})

	ctx, cancel := context.WithTimeout(r.Context(), 600*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(w, "data: %s\n\n", jsonErr(err.Error()))
		flusher.Flush()
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
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

	scanner := bufio.NewScanner(resp.Body)
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

	baseURL := getOllamaBaseURL()
	body, _ := json.Marshal(map[string]string{"name": model})

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "DELETE", baseURL+"/api/delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		addFlash(w, r, "error", fmt.Sprintf("Could not delete model: %v", err))
	} else {
		resp.Body.Close()
		if resp.StatusCode == 200 || resp.StatusCode == 404 {
			addFlash(w, r, "success", fmt.Sprintf("Model '%s' deleted.", model))
		} else {
			addFlash(w, r, "error", fmt.Sprintf("Ollama returned %d", resp.StatusCode))
		}
	}
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

func handleOllamaSettings(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	r.ParseForm()
	ollamaURL := strings.TrimSpace(r.FormValue("ollama_url"))
	port := strings.TrimSpace(r.FormValue("port"))

	if ollamaURL == "" || port == "" {
		addFlash(w, r, "error", "URL and port are required")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	portInt, err := strconv.Atoi(port)
	if err != nil || portInt < 1 || portInt > 65535 {
		addFlash(w, r, "error", "Port must be a number between 1 and 65535")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	setSetting("ollama_url", ollamaURL)
	setSetting("ollama_port", port)
	slog.Info("Ollama settings updated", "url", ollamaURL, "port", port)
	addFlash(w, r, "success", "Ollama settings saved")
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

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")

	baseURL := getOllamaBaseURL()
	body, _ := json.Marshal(map[string]interface{}{"name": model, "stream": true})

	ctx, cancel := context.WithTimeout(r.Context(), 600*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(w, "data: %s\n\n", jsonErr(err.Error()))
		flusher.Flush()
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
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

	scanner := bufio.NewScanner(resp.Body)
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
	addFlash(w, r, "success", "Password updated successfully")
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
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
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	page := maxInt(1, queryInt(r, "page", 1))

	rows, err := getLogs(search, 10_000)
	if err != nil {
		slog.Error("getLogs", "err", err)
		jsonResponse(w, 500, map[string]string{"error": "Internal server error"})
		return
	}

	total := len(rows)
	pages := maxInt(1, (total+9)/10)
	page = minInt(page, pages)
	start := (page - 1) * 10
	end := minInt(start+10, total)
	slice := rows[start:end]

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
	rows, err := getLogs("", 10_000_000)
	if err != nil {
		slog.Error("getLogs all", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=all_logs.csv")
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	cw.Write([]string{"log_id", "client_name", "model", "prompt", "response", "timestamp"})
	for _, row := range rows {
		cw.Write([]string{
			strconv.Itoa(row.ID), row.Name, row.Model,
			row.Prompt, row.Response, row.TS,
		})
	}
	cw.Flush()
	slog.Info("CSV export all logs")
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
	user, err := getAuthenticatedUser(r)
	if err != nil || user == nil {
		jsonResponse(w, 401, openaiError("Invalid API key", "authentication_error"))
		return
	}

	baseURL := getOllamaBaseURL()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(baseURL + "/api/tags")
	if err != nil {
		slog.Error("Ollama /api/tags", "err", err)
		jsonResponse(w, 502, openaiError("Could not reach Ollama", "api_error"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		jsonResponse(w, 502, openaiError("Upstream error", "api_error"))
		return
	}
	var tags map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tags)
	jsonResponse(w, 200, ollamaTagsToOpenAIModels(tags))
}

// ─────────────────────────────── API: chat completions ────────────────────────

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	user, err := getAuthenticatedUser(r)
	if err != nil || user == nil {
		jsonResponse(w, 401, openaiError("Invalid API key", "authentication_error"))
		return
	}

	if !isAllowed(user.ID, user.RateLimit) {
		jsonResponse(w, 429, openaiError("Rate limit exceeded", "rate_limit_error"))
		return
	}

	var data map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil || data == nil {
		jsonResponse(w, 400, openaiError("Invalid JSON body", "invalid_request_error"))
		return
	}
	if _, ok := data["messages"]; !ok {
		jsonResponse(w, 400, openaiError("'messages' is required", "invalid_request_error"))
		return
	}

	rawMsgs, _ := data["messages"].([]interface{})
	messages := sanitizeMessages(rawMsgs)
	ollamaReq := openAIToOllama(data, messages)
	baseURL := getOllamaBaseURL()
	chatURL := baseURL + "/api/chat"
	model, _ := ollamaReq["model"].(string)

	streaming, _ := data["stream"].(bool)
	if streaming {
		streamCompletions(w, r, chatURL, ollamaReq, user.ID, model)
		return
	}

	// Non-streaming
	body, _ := json.Marshal(ollamaReq)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", chatURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("Ollama request", "err", err)
		jsonResponse(w, 502, openaiError("Could not reach Ollama", "api_error"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		slog.Error("Ollama HTTP error", "status", resp.StatusCode)
		jsonResponse(w, 502, openaiError("Upstream error", "api_error"))
		return
	}

	var ollamaResp map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&ollamaResp)
	result := ollamaToOpenAI(ollamaResp)

	go logInteraction(user.ID, fmt.Sprintf("%v", messages), fmt.Sprintf("%v", result), model)
	jsonResponse(w, 200, result)
}

func streamCompletions(w http.ResponseWriter, r *http.Request, chatURL string, ollamaReq map[string]interface{}, userID int, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")

	body, _ := json.Marshal(ollamaReq)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", chatURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("stream request", "err", err)
		b, _ := json.Marshal(openaiError("Internal error", "api_error"))
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := json.Marshal(openaiError("Upstream error", "api_error"))
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
		return
	}

	completionID := newCompletionID()
	isFirst := true

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		openaiChunk := formatStreamChunk(chunk, completionID, isFirst)
		isFirst = false
		b, _ := json.Marshal(openaiChunk)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()

		if done, _ := chunk["done"].(bool); done {
			break
		}
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	go logInteraction(userID, fmt.Sprintf("%v", ollamaReq["messages"]), "streamed", model)
}

// ─────────────────────────────── Auth helper ──────────────────────────────────

func getAuthenticatedUser(r *http.Request) (*User, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil, nil
	}
	apiKey := auth[7:]
	user, err := getUserByAPIKey(apiKey)
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
