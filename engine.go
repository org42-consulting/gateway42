package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sharedTransport pools idle TCP connections across all engine adapters.
// Streaming and non-streaming requests reuse this transport; per-call
// timeouts are applied via context.WithTimeout rather than client.Timeout.
var sharedTransport = &http.Transport{
	MaxIdleConns:          200,
	MaxIdleConnsPerHost:   32,
	MaxConnsPerHost:       64,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}

// sharedClient has no Timeout — long-lived streaming responses must not
// be cut off arbitrarily. Use context.WithTimeout for per-call deadlines.
var sharedClient = &http.Client{Transport: sharedTransport}

// controlClient applies a 5s deadline at the client level. Use for short
// metadata calls (Status, ListModels, /v1/models on OpenAI-compat) where
// the entire roundtrip including body read must complete quickly.
var controlClient = &http.Client{Transport: sharedTransport, Timeout: 5 * time.Second}

// maxStreamLineSize bounds a single SSE/JSONL line. The default bufio.Scanner
// 64 KiB cap silently drops larger lines, which can happen when a model emits
// a long token chunk or embedded base64.
const maxStreamLineSize = 4 << 20 // 4 MiB

// newStreamScanner returns a bufio.Scanner sized for streaming line input.
func newStreamScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), maxStreamLineSize)
	return s
}

// marshalAudit renders v as JSON for audit logging. Falls back to a Go-format
// string on marshal failure so we never lose the log entry.
func marshalAudit(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// ── Engine cache ──────────────────────────────────────────────────────────────

type engineCache struct {
	engines  []EngineConfig
	adapters map[int]Engine // keyed by engine ID
}

var engineCachePtr atomic.Pointer[engineCache]

// reloadEngineCache rebuilds the cached engines+adapters from the DB.
// Call after any successful saveEngines() and once during startup.
func reloadEngineCache() error {
	engines, err := getEngines()
	if err != nil {
		// Empty cache is valid (no engines configured yet).
		engineCachePtr.Store(&engineCache{engines: nil, adapters: map[int]Engine{}})
		return err
	}
	adapters := make(map[int]Engine, len(engines))
	for _, e := range engines {
		if a, err := newEngine(e); err == nil {
			adapters[e.ID] = a
		}
	}
	engineCachePtr.Store(&engineCache{engines: engines, adapters: adapters})
	return nil
}

// cachedEngines returns the current snapshot of engine configs, or nil.
func cachedEngines() []EngineConfig {
	c := engineCachePtr.Load()
	if c == nil {
		return nil
	}
	return c.engines
}

// cachedAdapter returns the cached adapter for an engine ID, or nil.
func cachedAdapter(id int) Engine {
	c := engineCachePtr.Load()
	if c == nil {
		return nil
	}
	return c.adapters[id]
}

// cachedAdaptersByType returns all cached adapters of the given type, in the
// same order as the engine list.
func cachedAdaptersByType(t string) []Engine {
	c := engineCachePtr.Load()
	if c == nil {
		return nil
	}
	out := make([]Engine, 0, len(c.engines))
	for _, e := range c.engines {
		if e.Type == t {
			if a := c.adapters[e.ID]; a != nil {
				out = append(out, a)
			}
		}
	}
	return out
}

// Engine type constants
const (
	EngineOllama       = "ollama"
	EngineOpenAICompat = "openai_compat"
)

// EngineConfig holds per-engine settings, stored as JSON in the settings table.
type EngineConfig struct {
	ID       int    `json:"id,omitempty"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	BaseURL  string `json:"base_url"`
	Port     int    `json:"port,omitempty"`     // for Ollama, default 11434
	APIKey   string `json:"api_key,omitempty"`  // for openai_compat engines
}

// Engine is the interface all model engine adapters must implement.
type Engine interface {
	Type() string
	Name() string
	Status() (bool, error)                                                                // health check
	ListModels() ([]map[string]interface{}, error)                                        // list available models
	Chat(req map[string]interface{}) (map[string]interface{}, error)                      // non-streaming
	ChatStream(w http.ResponseWriter, r *http.Request, req map[string]interface{}, userID int, modelName string) // streaming
	baseURL() string                                                                      // host:port for Ollama-only ops

	// Upstream concurrency control. Acquire blocks until a slot is free OR
	// ctx fires; Release returns the slot. Always pair them with defer.
	TryAcquire(ctx context.Context) bool
	Release()
}

// concurrencyLimiter caps simultaneous in-flight upstream requests per engine.
// One Ollama instance cannot serve many parallel streams without thrashing;
// the cap returns 503 instead of letting the GPU OOM.
type concurrencyLimiter struct {
	sem chan struct{}
}

func newConcurrencyLimiter(n int) *concurrencyLimiter {
	if n <= 0 {
		n = 8
	}
	return &concurrencyLimiter{sem: make(chan struct{}, n)}
}

func (c *concurrencyLimiter) TryAcquire(ctx context.Context) bool {
	select {
	case c.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *concurrencyLimiter) Release() {
	select {
	case <-c.sem:
	default:
	}
}

// ── Engine CRUD ───────────────────────────────────────────────────────────────

// getEngines returns all configured engines from the DB.
func getEngines() ([]EngineConfig, error) {
	var val string
	err := dbRead.QueryRow("SELECT value FROM settings WHERE key=?", "engines").Scan(&val)
	if err != nil {
		return nil, err
	}
	if val == "" {
		return nil, nil
	}
	var engines []EngineConfig
	if err := json.Unmarshal([]byte(val), &engines); err != nil {
		return nil, err
	}
	return engines, nil
}

// saveEngines persists the engine list to the DB settings table and refreshes
// the in-memory engine/adapter cache so subsequent requests see the change.
func saveEngines(engines []EngineConfig) error {
	b, err := json.Marshal(engines)
	if err != nil {
		return err
	}
	if err := setSetting("engines", string(b)); err != nil {
		return err
	}
	invalidateProbeCache()
	return reloadEngineCache()
}

// newEngine creates an Engine implementation from a config.
func newEngine(cf EngineConfig) (Engine, error) {
	cl := newConcurrencyLimiter(cfg.UpstreamConcurrency)
	switch cf.Type {
	case EngineOllama:
		return &OllamaAdapter{cfg: cf, concurrencyLimiter: cl}, nil
	case EngineOpenAICompat:
		return &OpenAICompatAdapter{cfg: cf, concurrencyLimiter: cl}, nil
	default:
		return nil, fmt.Errorf("unknown engine type: %s", cf.Type)
	}
}

// GetEngineByName returns the EngineConfig for an engine by its database ID, or nil.
func GetEngineByName(engines []EngineConfig, id int) *EngineConfig {
	for _, e := range engines {
		if e.ID == id {
			return &e
		}
	}
	return nil
}

// GetFirstOllamaAdapter returns an adapter for the first Ollama engine, or nil if none configured.
func GetFirstOllamaAdapter() (Engine, error) {
	engines, err := getEngines()
	if err != nil || len(engines) == 0 {
		return nil, err
	}
	for _, e := range engines {
		if e.Type == EngineOllama {
			return newEngine(e)
		}
	}
	return nil, fmt.Errorf("no Ollama engine configured")
}

// modelName extracts the model identifier from a model map, handling both
// Ollama ("name") and OpenAI-compat ("id") response formats.
func modelName(m map[string]interface{}) string {
	if n, _ := m["name"].(string); n != "" {
		return n
	}
	n, _ := m["id"].(string)
	return n
}

// probeEngine probes an engine adapter for status and model names.
func probeEngine(e Engine) (bool, []string) {
	status, err := e.Status()
	if err != nil || !status {
		return false, nil
	}
	models, err := e.ListModels()
	if err != nil {
		return true, nil
	}
	var names []string
	for _, m := range models {
		if n := modelName(m); n != "" {
			names = append(names, n)
		}
	}
	return true, names
}

// ── Probe cache ───────────────────────────────────────────────────────────────

const probeCacheTTL = 10 * time.Second

type probeEntry struct {
	status  bool
	models  []string
	details []ModelDetail
	expiry  time.Time
}

var (
	probeCacheMu sync.Mutex
	probeCache   = map[int]probeEntry{}
)

// probeEngineCached returns probe data for the given engine ID, refreshing
// from upstream only if the cache is stale.
func probeEngineCached(id int, e Engine) (bool, []string, []ModelDetail) {
	probeCacheMu.Lock()
	if entry, ok := probeCache[id]; ok && time.Now().Before(entry.expiry) {
		probeCacheMu.Unlock()
		return entry.status, entry.models, entry.details
	}
	probeCacheMu.Unlock()

	status, models, details := probeEngineFull(e)

	probeCacheMu.Lock()
	probeCache[id] = probeEntry{status, models, details, time.Now().Add(probeCacheTTL)}
	probeCacheMu.Unlock()
	return status, models, details
}

// invalidateProbeCache clears the probe cache. Call after engine edits so
// the admin sees fresh data immediately.
func invalidateProbeCache() {
	probeCacheMu.Lock()
	probeCache = map[int]probeEntry{}
	probeCacheMu.Unlock()
}

// warmProbeCache runs probes for all engines in parallel, returning when
// every probe finishes or ctx fires, whichever is first. Stale entries are
// refreshed; fresh ones are skipped.
func warmProbeCache(ctx context.Context) {
	engines := cachedEngines()
	if len(engines) == 0 {
		return
	}
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, e := range engines {
			a := cachedAdapter(e.ID)
			if a == nil {
				continue
			}
			wg.Add(1)
			go func(id int, ad Engine) {
				defer wg.Done()
				probeEngineCached(id, ad)
			}(e.ID, a)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// Goroutines keep running; their results will populate the cache
		// for the next page load.
	}
}

// probeEngineFull probes an engine adapter for status, model names, and model details.
func probeEngineFull(e Engine) (bool, []string, []ModelDetail) {
	status, err := e.Status()
	if err != nil || !status {
		return false, nil, nil
	}
	models, err := e.ListModels()
	if err != nil {
		return true, nil, nil
	}
	var names []string
	var details []ModelDetail
	for _, m := range models {
		n := modelName(m)
		if n == "" {
			continue
		}
		names = append(names, n)
		sizeB, _ := m["size"].(float64)
		var sizeStr string
		if sizeB >= 1_000_000_000 {
			sizeStr = fmt.Sprintf("%.1f GB", sizeB/1e9)
		} else if sizeB > 0 {
			sizeStr = fmt.Sprintf("%d MB", int(sizeB)/1_000_000)
		}
		details = append(details, ModelDetail{Name: n, Size: sizeStr})
	}
	return true, names, details
}

// OllamaEndpointFromConfig returns the host:port endpoint for an Ollama config.
func OllamaEndpointFromConfig(cfg EngineConfig) string {
	host := cfg.BaseURL
	port := cfg.Port
	if port == 0 {
		port = 11434
	}
	return fmt.Sprintf("%s:%d", strings.TrimRight(host, "/"), port)
}

// GetOllamaBaseURL returns the base URL and port for the first Ollama engine.
func GetOllamaBaseURL() (string, int) {
	engines, err := getEngines()
	if err != nil || len(engines) == 0 {
		return "127.0.0.1", 11434
	}
	for _, e := range engines {
		if e.Type == EngineOllama {
			return e.BaseURL, e.Port
		}
	}
	return "127.0.0.1", 11434
}

// ─────────────────── OllamaAdapter ────────────────────────────────────────────

type OllamaAdapter struct {
	cfg EngineConfig
	*concurrencyLimiter
}

func (a *OllamaAdapter) Type() string  { return EngineOllama }
func (a *OllamaAdapter) Name() string  { return a.cfg.Name }

func (a *OllamaAdapter) baseURL() string {
	u := strings.TrimRight(a.cfg.BaseURL, "/")
	strip := strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
	_, _, err := net.SplitHostPort(strip)
	if err == nil {
		// URL already contains a port — use as-is.
		return u
	}

	port := a.cfg.Port
	if port == 0 {
		port = 11434
	}
	return fmt.Sprintf("%s:%d", u, port)
}

func (a *OllamaAdapter) Status() (bool, error) {
	resp, err := controlClient.Get(a.baseURL() + "/api/version")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200, nil
}

func (a *OllamaAdapter) ListModels() ([]map[string]interface{}, error) {
	resp, err := controlClient.Get(a.baseURL() + "/api/tags")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tags map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	models, _ := tags["models"].([]interface{})
	result := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		if mm, ok := m.(map[string]interface{}); ok {
			result = append(result, mm)
		}
	}
	return result, nil
}

func (a *OllamaAdapter) Chat(req map[string]interface{}) (map[string]interface{}, error) {
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var result map[string]interface{}
	err := observeUpstream(EngineOllama, "chat", func() error {
		httpReq, _ := http.NewRequestWithContext(ctx, "POST", a.baseURL()+"/api/chat", bytes.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := sharedClient.Do(httpReq)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("ollama returned %d", resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(&result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (a *OllamaAdapter) ChatStream(w http.ResponseWriter, r *http.Request, req map[string]interface{}, userID int, modelName string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")

	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", a.baseURL()+"/api/chat", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := sharedClient.Do(httpReq)
	if err != nil {
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

	scanner := newStreamScanner(resp.Body)
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

	logInteraction(userID, marshalAudit(req["messages"]), "streamed", modelName)
}

// ─────────────────── OpenAICompatAdapter ──────────────────────────────────────

type OpenAICompatAdapter struct {
	cfg EngineConfig
	*concurrencyLimiter
}

func (a *OpenAICompatAdapter) Type() string { return EngineOpenAICompat }
func (a *OpenAICompatAdapter) Name() string { return a.cfg.Name }

func (a *OpenAICompatAdapter) baseURL() string {
	return strings.TrimRight(a.upstream(""), "/")
}

func (a *OpenAICompatAdapter) upstream(path string) string {
	u := a.cfg.BaseURL
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	u = strings.TrimRight(u, "/")
	if path == "" || path == "/" {
		return u
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return u + path
}

func (a *OpenAICompatAdapter) doReq(method, path string, body io.Reader) (*http.Response, error) {
	r, err := http.NewRequest(method, a.upstream(path), body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	if a.cfg.APIKey != "" {
		r.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	}
	return controlClient.Do(r)
}

func (a *OpenAICompatAdapter) Status() (bool, error) {
	resp, err := a.doReq("GET", "/v1/models", nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200, nil
}

func (a *OpenAICompatAdapter) ListModels() ([]map[string]interface{}, error) {
	resp, err := a.doReq("GET", "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	data, _ := result["data"].([]interface{})
	models := make([]map[string]interface{}, 0, len(data))
	for _, m := range data {
		if mm, ok := m.(map[string]interface{}); ok {
			models = append(models, mm)
		}
	}
	return models, nil
}

func (a *OpenAICompatAdapter) Chat(req map[string]interface{}) (map[string]interface{}, error) {
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var result map[string]interface{}
	err := observeUpstream(EngineOpenAICompat, "chat", func() error {
		httpReq, _ := http.NewRequestWithContext(ctx, "POST", a.upstream("/v1/chat/completions"), bytes.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		if a.cfg.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
		}
		resp, err := sharedClient.Do(httpReq)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("engine returned %d", resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(&result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (a *OpenAICompatAdapter) ChatStream(w http.ResponseWriter, r *http.Request, req map[string]interface{}, userID int, modelName string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")

	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", a.upstream("/v1/chat/completions"), bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	if a.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	}

	resp, err := sharedClient.Do(httpReq)
	if err != nil {
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

	scanner := newStreamScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fmt.Fprintf(w, "%s\n\n", line)
		flusher.Flush()
		if line == "data: [DONE]" {
			break
		}
	}
	flusher.Flush()

	logInteraction(userID, marshalAudit(req["messages"]), "streamed", modelName)
}

// ── Ollama-only helpers (search, pull, delete) ────────────────────────────────

// OllamaSearchModels searches ollama.com for models
func OllamaSearchModels(query string) ([]ModelDetail, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("https://ollama.com/search?q=%s", url.QueryEscape(query)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	re := regexp.MustCompile(`href="/(?:library/)?([^"/]+(?:/[^"/]+)?)\"[^>]*class="group w-full"`)
	matches := re.FindAllStringSubmatch(string(bodyBytes), -1)

	modelMap := make(map[string]bool)
	var results []ModelDetail
	for _, m := range matches {
		if len(m) > 1 && !modelMap[m[1]] {
			modelMap[m[1]] = true
			results = append(results, ModelDetail{Name: m[1], Size: "N/A"})
		}
	}
	return results, nil
}

// OllamaPullModel streams a model pull from Ollama as SSE.
func OllamaPullModel(baseURL string, port int, model string, w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")

	endpoint := fmt.Sprintf("%s:%d", strings.TrimRight(baseURL, "/"), port)
	body, _ := json.Marshal(map[string]interface{}{"name": model, "stream": true})

	ctx, cancel := context.WithTimeout(r.Context(), 600*time.Second)
	defer cancel()

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", endpoint+"/api/pull", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := sharedClient.Do(httpReq)
	if err != nil {
		fmt.Fprintf(w, "data: %s\n\n", jsonErr(err.Error()))
		flusher.Flush()
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Fprintf(w, "data: %s\n\n", jsonErr(fmt.Sprintf("Engine returned %d", resp.StatusCode)))
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

// OllamaDeleteModel deletes a model from an Ollama instance.
func OllamaDeleteModel(baseURL string, port int, model string) error {
	endpoint := fmt.Sprintf("%s:%d", strings.TrimRight(baseURL, "/"), port)
	body, _ := json.Marshal(map[string]string{"name": model})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	httpReq, _ := http.NewRequestWithContext(ctx, "DELETE", endpoint+"/api/delete", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := sharedClient.Do(httpReq)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
