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

// upstreamChatTimeout caps a single completion. It is an upper bound for a
// wedged engine, not a target: the request context is the primary deadline, so
// a client that hangs up releases the engine's concurrency slot immediately
// rather than holding it for the remainder of this budget.
const upstreamChatTimeout = 120 * time.Second

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
	ID      int    `json:"id,omitempty"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	Port    int    `json:"port,omitempty"`    // for Ollama, default 11434
	APIKey  string `json:"api_key,omitempty"` // for openai_compat engines
}

// Engine is the interface all model engine adapters must implement.
type Engine interface {
	Type() string
	Name() string
	Status() (bool, error)                                                                // health check
	ListModels() ([]map[string]interface{}, error)                                        // list available models
	Chat(ctx context.Context, req map[string]interface{}) (map[string]interface{}, error) // non-streaming
	StreamChat(ctx context.Context, req map[string]interface{}) (ChatStream, error)       // streaming
	baseURL() string                                                                      // host:port for Ollama-only ops

	// Upstream concurrency control. Acquire blocks until a slot is free OR
	// ctx fires; Release returns the slot. Always pair them with defer.
	TryAcquire(ctx context.Context) bool
	Release()
}

// ChatStream is a live streaming completion. Recv returns successive SSE data
// payloads — the bytes that belong after "data: " — already in OpenAI wire
// shape, and io.EOF once the upstream stream ends normally. Close must always
// be called, and releases the underlying response body.
//
// This replaces a ChatStream(w, r, …) method that took an http.ResponseWriter.
// Handing adapters the writer meant SSE framing, error payloads, flushing and
// audit logging were reimplemented per engine type, and the translation could
// not be exercised without standing up an httptest recorder and parsing the
// framing back off. With a reader the handler owns the wire format once and a
// test can drive an adapter's translator straight from a bytes.Reader.
type ChatStream interface {
	Recv() ([]byte, error)
	Close() error
}

// ollamaStream translates Ollama's newline-delimited JSON into OpenAI chunks.
type ollamaStream struct {
	body         io.ReadCloser
	scanner      *bufio.Scanner
	completionID string
	isFirst      bool
	done         bool
}

func (s *ollamaStream) Recv() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			// One unparseable line does not justify tearing down the stream:
			// Ollama emits a self-contained JSON object per line, so the next
			// one is still usable.
			continue
		}
		out, err := json.Marshal(formatStreamChunk(chunk, s.completionID, s.isFirst))
		if err != nil {
			return nil, err
		}
		s.isFirst = false
		// Report this chunk, then stop on the next call — the final Ollama
		// object carries both done:true and the usage counts, so returning
		// EOF here instead would discard them.
		if fin, _ := chunk["done"].(bool); fin {
			s.done = true
		}
		return out, nil
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (s *ollamaStream) Close() error { return s.body.Close() }

// passthroughStream forwards an upstream that already speaks OpenAI SSE. It
// unwraps the "data: " framing so the handler can re-apply it uniformly for
// both engine types. Non-data lines (SSE comments, "event:", "id:") are
// dropped: this gateway emits nothing but data frames, and upstream keepalive
// comments have no meaning once our own writes drive the flush cadence.
type passthroughStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	done    bool
}

func (s *passthroughStream) Recv() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	for s.scanner.Scan() {
		payload, ok := strings.CutPrefix(strings.TrimSpace(s.scanner.Text()), "data:")
		if !ok {
			continue
		}
		if payload = strings.TrimSpace(payload); payload == "" {
			continue
		}
		if payload == "[DONE]" {
			// Swallow the upstream terminator; the handler emits its own, so
			// a stream that ends without one still terminates correctly.
			s.done = true
			return nil, io.EOF
		}
		return []byte(payload), nil
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (s *passthroughStream) Close() error { return s.body.Close() }

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

// modelName extracts the model identifier from a model map, handling both
// Ollama ("name") and OpenAI-compat ("id") response formats.
func modelName(m map[string]interface{}) string {
	if n, _ := m["name"].(string); n != "" {
		return n
	}
	n, _ := m["id"].(string)
	return n
}

// ── Probe cache ───────────────────────────────────────────────────────────────

const probeCacheTTL = 10 * time.Second

// probeData is everything one probe of an engine yields. raw keeps the
// unmodified ListModels payload so /v1/models can report per-model metadata
// (created, size, owner) without a second round of upstream calls — the probe
// already fetched it.
type probeData struct {
	status  bool
	models  []string
	details []ModelDetail
	raw     []map[string]interface{}
}

type probeEntry struct {
	probeData
	expiry time.Time

	// inflight is non-nil while a refresh is running for this engine and is
	// closed when that refresh finishes. Callers that find it set wait on it
	// instead of starting a second upstream probe.
	inflight chan struct{}
}

var (
	probeCacheMu sync.Mutex
	probeCache   = map[int]probeEntry{}
)

// probeEngineCached returns probe data for the given engine ID, refreshing
// from upstream only if the cache is stale.
//
// Concurrent callers for the same engine collapse onto a single upstream
// probe: the first caller owns the refresh and the rest block on its
// completion channel. Without that, one cold cache entry plus a burst of
// dashboard loads would open one connection per request to an engine that
// is, by definition, already slow to answer.
func probeEngineCached(id int, e Engine) probeData {
	for {
		probeCacheMu.Lock()
		entry, ok := probeCache[id]
		if ok && entry.inflight != nil {
			// Someone else is already refreshing — wait, then re-read.
			wait := entry.inflight
			probeCacheMu.Unlock()
			<-wait
			continue
		}
		if ok && time.Now().Before(entry.expiry) {
			probeCacheMu.Unlock()
			return entry.probeData
		}
		// We own the refresh. Publish the in-flight marker, preserving any
		// stale values already in the entry.
		done := make(chan struct{})
		entry.inflight = done
		probeCache[id] = entry
		probeCacheMu.Unlock()
		return refreshProbe(id, e, done)
	}
}

// refreshProbe probes upstream, stores the result, and releases callers
// blocked on done. If the probe panics it clears the entry rather than
// leaving an in-flight marker behind a closed channel, which would spin
// waiters forever.
func refreshProbe(id int, e Engine, done chan struct{}) probeData {
	stored := false
	defer func() {
		if !stored {
			probeCacheMu.Lock()
			delete(probeCache, id)
			probeCacheMu.Unlock()
		}
		close(done)
	}()

	data := probeEngineFull(e)

	probeCacheMu.Lock()
	probeCache[id] = probeEntry{
		probeData: data,
		expiry:    time.Now().Add(probeCacheTTL),
	}
	stored = true
	probeCacheMu.Unlock()
	return data
}

// invalidateProbeCache clears the probe cache. Call after engine edits so
// the admin sees fresh data immediately.
func invalidateProbeCache() {
	probeCacheMu.Lock()
	probeCache = map[int]probeEntry{}
	probeCacheMu.Unlock()
	// The routing index is derived from probe data, so it is stale the moment
	// the probes are. Dropping it here is what makes a freshly pulled model
	// routable immediately instead of up to routeIndexTTL later.
	invalidateRouteIndex()
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
func probeEngineFull(e Engine) probeData {
	status, err := e.Status()
	if err != nil || !status {
		return probeData{}
	}
	models, err := e.ListModels()
	if err != nil {
		return probeData{status: true}
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
	return probeData{status: true, models: names, details: details, raw: models}
}

// ─────────────────── OllamaAdapter ────────────────────────────────────────────

type OllamaAdapter struct {
	cfg EngineConfig
	*concurrencyLimiter
}

func (a *OllamaAdapter) Type() string { return EngineOllama }
func (a *OllamaAdapter) Name() string { return a.cfg.Name }

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

func (a *OllamaAdapter) Chat(ctx context.Context, req map[string]interface{}) (map[string]interface{}, error) {
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, upstreamChatTimeout)
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

// StreamChat opens a streaming completion. The caller owns ctx — including its
// deadline — because the returned stream outlives this call.
func (a *OllamaAdapter) StreamChat(ctx context.Context, req map[string]interface{}) (ChatStream, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.baseURL()+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := sharedClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		// Not deferred because the success path hands this body to the stream
		// below and must not close it. The close error is dropped on purpose:
		// we are already returning the failure the caller needs to see, and
		// replacing it with a close error would hide the status code.
		_ = resp.Body.Close()
		return nil, fmt.Errorf("engine returned %d", resp.StatusCode)
	}
	return &ollamaStream{
		body:         resp.Body,
		scanner:      newStreamScanner(resp.Body),
		completionID: newCompletionID(),
		isFirst:      true,
	}, nil
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

func (a *OpenAICompatAdapter) Chat(ctx context.Context, req map[string]interface{}) (map[string]interface{}, error) {
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, upstreamChatTimeout)
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

// StreamChat opens a streaming completion. The upstream already speaks OpenAI
// SSE, so no translation is needed — only unwrapping of the frames.
func (a *OpenAICompatAdapter) StreamChat(ctx context.Context, req map[string]interface{}) (ChatStream, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.upstream("/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if a.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	}

	resp, err := sharedClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		// Not deferred because the success path hands this body to the stream
		// below and must not close it. The close error is dropped on purpose:
		// we are already returning the failure the caller needs to see, and
		// replacing it with a close error would hide the status code.
		_ = resp.Body.Close()
		return nil, fmt.Errorf("engine returned %d", resp.StatusCode)
	}
	return &passthroughStream{body: resp.Body, scanner: newStreamScanner(resp.Body)}, nil
}

// ── Ollama-only helpers (search, pull, delete) ────────────────────────────────

// ollamaSearchLinkRe matches the model links on an ollama.com search page,
// e.g. <a href="/library/llama3.2" class="group w-full"> and namespaced
// entries like <a href="/batiai/qwen3.6-35b" class="group w-full">.
var ollamaSearchLinkRe = regexp.MustCompile(`href="/(?:library/)?([^"/]+(?:/[^"/]+)?)\"[^>]*class="group w-full"`)

// OllamaSearchModels scrapes ollama.com/search for models matching query.
// This parses HTML, so it is inherently brittle to upstream redesigns; an
// empty result set is far more likely than an error when that happens.
func OllamaSearchModels(query string) ([]ModelDetail, error) {
	resp, err := controlClient.Get(fmt.Sprintf("https://ollama.com/search?q=%s", url.QueryEscape(query)))
	if err != nil {
		return nil, fmt.Errorf("could not reach ollama.com: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama.com search returned %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading ollama.com response: %w", err)
	}
	matches := ollamaSearchLinkRe.FindAllStringSubmatch(string(bodyBytes), -1)

	seen := make(map[string]bool, len(matches))
	results := make([]ModelDetail, 0, len(matches))
	for _, m := range matches {
		if len(m) > 1 && !seen[m[1]] {
			seen[m[1]] = true
			// Size is not exposed on the search page.
			results = append(results, ModelDetail{Name: m[1], Size: "N/A"})
		}
	}
	return results, nil
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
	defer resp.Body.Close()
	return nil
}
