package main

// oMLX model management.
//
// oMLX is the gateway's first-class engine. Beyond the OpenAI-compatible
// inference API every engine shares, the admin pages manage its models
// through oMLX's own API, authenticated with the engine's API key (oMLX
// accepts its main API key as a Bearer token on its admin routes):
//
//   - status:   GET  /v1/models/status            loaded / loading, size
//   - search:   GET  /admin/api/hf/search          Hugging Face, MLX models
//   - download: POST /admin/api/hf/download        a background task, whose
//     progress is GET /admin/api/hf/tasks
//   - delete:   DELETE /admin/api/hf/models/{id}   weights removed from disk
//   - load / unload: POST /v1/models/{id}/load | /unload
//
// These replace the Ollama search (ollama.com), pull and delete the gateway
// had before Ollama support was replaced by oMLX. Downloads put weights on
// the engine's disk; the admin session (and CSRF) guards every action.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// omlxRequest sends an authenticated request to an oMLX engine. path is
// relative to the engine's root (both /v1/… and /admin/… live there).
func omlxRequest(ctx context.Context, a *OpenAICompatAdapter, method, path string, body interface{}) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.upstream(path), rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	}
	return completionClient.Do(req)
}

// omlxJSON is omlxRequest that decodes a 2xx JSON answer into out (when not
// nil) and turns any other status into an upstreamStatusError.
func omlxJSON(ctx context.Context, a *OpenAICompatAdapter, method, path string, body, out interface{}) error {
	resp, err := omlxRequest(ctx, a, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newUpstreamStatusError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// omlxEngine returns the oMLX engine an admin action targets: the one with
// wantID, or — only when no ID was given — the first oMLX engine. A wrong ID
// never falls back: with several oMLX hosts, guessing would download to, or
// delete from, the wrong machine.
func omlxEngine(wantID int) *OpenAICompatAdapter {
	for _, e := range cachedEngines() {
		if e.Type != EngineOMLX || (wantID != 0 && e.ID != wantID) {
			continue
		}
		if a, ok := cachedAdapter(e.ID).(*OpenAICompatAdapter); ok {
			return a
		}
	}
	return nil
}

// ── Model status ─────────────────────────────────────────────────────────────

type omlxModelStatus struct {
	ID            string `json:"id"`
	Loaded        bool   `json:"loaded"`
	IsLoading     bool   `json:"is_loading"`
	EstimatedSize int64  `json:"estimated_size"`
}

// omlxModelStatuses reads which of an engine's models are loaded.
func omlxModelStatuses(ctx context.Context, a *OpenAICompatAdapter) (map[string]omlxModelStatus, error) {
	var body struct {
		Models []omlxModelStatus `json:"models"`
	}
	if err := omlxJSON(ctx, a, http.MethodGet, "/v1/models/status", nil, &body); err != nil {
		return nil, err
	}
	out := make(map[string]omlxModelStatus, len(body.Models))
	for _, m := range body.Models {
		out[m.ID] = m
	}
	return out, nil
}

// withOMLXStatus marks each model detail of an oMLX engine as manageable and
// loaded or not, filling in a size the listing lacked. Without status (the
// engine did not answer in time) the details are returned unmarked.
func withOMLXStatus(ctx context.Context, a *OpenAICompatAdapter, details []ModelDetail) []ModelDetail {
	statuses, err := omlxModelStatuses(ctx, a)
	out := make([]ModelDetail, len(details))
	for i, d := range details {
		d.Manageable = true
		if err == nil {
			s := statuses[d.Name]
			d.Loaded, d.Loading = s.Loaded, s.IsLoading
			if d.Size == "" && s.EstimatedSize > 0 {
				d.Size = formatBytes(s.EstimatedSize)
			}
		}
		out[i] = d
	}
	return out
}

func formatBytes(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%d MB", n/1_000_000)
	case n > 0:
		return fmt.Sprintf("%d KB", n/1_000)
	}
	return ""
}

// ── Search ───────────────────────────────────────────────────────────────────

// handleOMLXSearch serves GET /admin/omlx-search?q=…[&engine_id=…]: MLX
// models on Hugging Face, searched by the oMLX engine.
func handleOMLXSearch(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "Search query required"})
		return
	}
	engineID, _ := strconv.Atoi(r.URL.Query().Get("engine_id"))
	a := omlxEngine(engineID)
	if a == nil {
		jsonResponse(w, http.StatusOK, map[string]string{"error": "No oMLX engine configured"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var body struct {
		Models []struct {
			RepoID        string `json:"repo_id"`
			SizeFormatted string `json:"size_formatted"`
			Downloads     int    `json:"downloads"`
		} `json:"models"`
	}
	path := "/admin/api/hf/search?" + url.Values{"q": {q}, "limit": {"30"}, "sort": {"trending"}}.Encode()
	if err := omlxJSON(ctx, a, http.MethodGet, path, nil, &body); err != nil {
		slog.Warn("omlx search", "q", q, "engine", a.Name(), "err", err)
		jsonResponse(w, http.StatusOK, map[string]string{"error": omlxErrMessage(err)})
		return
	}
	results := make([]ModelDetail, 0, len(body.Models))
	for _, m := range body.Models {
		results = append(results, ModelDetail{Name: m.RepoID, Size: m.SizeFormatted, Downloads: m.Downloads})
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"results": results})
}

// omlxErrMessage words an engine failure for the admin page.
func omlxErrMessage(err error) string {
	var ue *upstreamStatusError
	if errors.As(err, &ue) {
		switch {
		case ue.Status == 401 || ue.Status == 403:
			return "oMLX refused the request: check the engine's API key (the main oMLX key is needed for model management)"
		case ue.Message != "":
			return ue.Message
		}
		return fmt.Sprintf("oMLX returned %d", ue.Status)
	}
	return "Could not reach oMLX: " + err.Error()
}

// ── Download ─────────────────────────────────────────────────────────────────

// omlxDownloadPoll is how often a download's progress is read.
var omlxDownloadPoll = time.Second

// handleOMLXDownloadStream serves GET /admin/omlx-download-stream?repo_id=…
// [&engine_id=…]&_csrf=…: it starts a Hugging Face download on the oMLX
// engine and streams its progress as SSE frames {status, completed, total},
// ending with {status: "success"} or {error}, then [DONE]. It is a GET only
// because EventSource cannot send anything else, which is why it checks the
// CSRF token itself: the middleware exempts GETs.
//
// The download is oMLX's background task: a closed page stops the progress
// stream, not the download.
func handleOMLXDownloadStream(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	if !csrfQueryValid(r) {
		slog.Warn("csrf: rejected download", "ip", clientIP(r))
		jsonResponse(w, http.StatusForbidden,
			map[string]string{"error": "CSRF token missing or invalid — reload the page and retry."})
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo_id"))
	if repo == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "Model (repo_id) required"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}
	// Headers first: every exit below writes an SSE frame, and a frame
	// written before these are set goes out as text/plain, which EventSource
	// rejects without ever showing the error it carried.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	frame := func(v interface{}) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	finish := func(v interface{}) {
		frame(v)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	engineID, _ := strconv.Atoi(r.URL.Query().Get("engine_id"))
	a := omlxEngine(engineID)
	if a == nil {
		finish(map[string]string{"error": "No matching oMLX engine configured"})
		return
	}

	var started struct {
		Task struct {
			TaskID string `json:"task_id"`
		} `json:"task"`
	}
	startCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	err := omlxJSON(startCtx, a, http.MethodPost, "/admin/api/hf/download", map[string]interface{}{"repo_id": repo}, &started)
	cancel()
	if err != nil {
		finish(map[string]string{"error": omlxErrMessage(err)})
		return
	}
	slog.Info("omlx download started", "repo", repo, "engine", a.Name(), "task", started.Task.TaskID)
	frame(map[string]interface{}{"status": "Download started"})

	// Whatever the download achieved, the engine's model list may change;
	// dropping the probe cache makes a finished model routable at once.
	defer onEnginesChanged()

	tick := time.NewTicker(omlxDownloadPoll)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
		var tasks struct {
			Tasks []struct {
				TaskID         string  `json:"task_id"`
				Status         string  `json:"status"`
				Progress       float64 `json:"progress"`
				TotalSize      int64   `json:"total_size"`
				DownloadedSize int64   `json:"downloaded_size"`
				Error          string  `json:"error"`
			} `json:"tasks"`
		}
		pollCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		err := omlxJSON(pollCtx, a, http.MethodGet, "/admin/api/hf/tasks", nil, &tasks)
		cancel()
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			finish(map[string]string{"error": "Lost track of the download: " + omlxErrMessage(err)})
			return
		}
		for _, t := range tasks.Tasks {
			if t.TaskID != started.Task.TaskID {
				continue
			}
			switch t.Status {
			case "completed":
				slog.Info("omlx download finished", "repo", repo, "engine", a.Name())
				finish(map[string]interface{}{"status": "success", "completed": t.TotalSize, "total": t.TotalSize})
				return
			case "failed", "cancelled":
				msg := t.Error
				if msg == "" {
					msg = "Download " + t.Status
				}
				finish(map[string]string{"error": msg})
				return
			}
			frame(map[string]interface{}{"status": t.Status, "completed": t.DownloadedSize, "total": t.TotalSize})
		}
	}
}

// ── Delete, load, unload ─────────────────────────────────────────────────────

// handleOMLXModelAction serves POST /admin/omlx-model-action with model,
// engine_id and action = load | unload | delete.
func handleOMLXModelAction(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/settings-page") {
		return
	}
	back := func(category, msg string) {
		addFlash(w, r, category, msg)
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
	}
	model := strings.TrimSpace(r.FormValue("model"))
	action := r.FormValue("action")
	if model == "" {
		back("error", "Model name is required")
		return
	}
	wantID, _ := strconv.Atoi(r.FormValue("engine_id"))
	a := omlxEngine(wantID)
	if a == nil {
		back("error", "No matching oMLX engine configured")
		return
	}
	escaped := url.PathEscape(model)
	var method, path, done string
	timeout := 30 * time.Second
	switch action {
	case "load":
		// Blocks until the model is in memory, which for a large one takes
		// a while.
		method, path, done, timeout = http.MethodPost, "/v1/models/"+escaped+"/load", "loaded", 10*time.Minute
	case "unload":
		method, path, done = http.MethodPost, "/v1/models/"+escaped+"/unload", "unloaded"
	case "delete":
		method, path, done = http.MethodDelete, "/admin/api/hf/models/"+escaped, "deleted"
	default:
		back("error", "Unknown model action")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	if err := omlxJSON(ctx, a, method, path, nil, nil); err != nil {
		slog.Error("omlx model action", "action", action, "model", model, "engine", a.Name(), "err", err)
		back("error", fmt.Sprintf("Could not %s '%s': %s", action, model, omlxErrMessage(err)))
		return
	}
	// The model list (delete) or its status (load, unload) changed.
	onEnginesChanged()
	slog.Info("omlx model action", "action", action, "model", model, "engine", a.Name())
	back("success", fmt.Sprintf("Model '%s' %s on %s.", model, done, a.Name()))
}
