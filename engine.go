package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
//
// DialContext is not boilerplate: without it the zero-value dialer has no
// connect timeout, so an engine whose host accepts TCP slowly (or black-holes
// packets) could sit in dial forever — ResponseHeaderTimeout only starts
// counting once the connection exists. 10s is generous for a LAN peer and
// still well under the 30s response-header budget.
var sharedTransport = &http.Transport{
	MaxIdleConns:          200,
	MaxIdleConnsPerHost:   32,
	MaxConnsPerHost:       64,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	// Upstream TLS engines get HTTP/2 multiplexing; plain HTTP is unaffected.
	ForceAttemptHTTP2: true,
}

// sharedClient has no Timeout — long-lived streaming responses must not
// be cut off arbitrarily. Use context.WithTimeout for per-call deadlines.
var sharedClient = &http.Client{Transport: sharedTransport}

// controlClient applies a 5s deadline at the client level. Use for short
// metadata calls (Status, ListModels, /v1/models on OpenAI-compat) where
// the entire roundtrip including body read must complete quickly.
var controlClient = &http.Client{Transport: sharedTransport, Timeout: 5 * time.Second}

// upstreamChatTimeout caps a single non-streaming completion. A non-streaming
// response sends nothing until it is finished, so elapsed time is the only
// progress signal there is and a total budget is the only bound that can catch
// a wedged engine. It is an upper bound, not a target; the request context is
// the primary deadline, so a client that hangs up releases the engine's
// concurrency slot immediately rather than holding it for the rest of this
// budget. Streaming completions are bounded by silence instead — see
// streamDeadline — because there a long generation is not a stalled one.
const upstreamChatTimeout = 120 * time.Second

// A stream is bounded by inactivity rather than by total duration. Measured on
// the local engine at ~58 generated tokens/s, 120s of wall clock is only ~6.9k
// output tokens, which a single agent completion legitimately exceeds; a wedged
// engine by contrast stops sending bytes. streamIdle is the gap that marks a
// stream dead, and streamMax is the backstop for an engine that dribbles bytes
// forever, since a stream holds a goroutine and one of the
// GW42_UPSTREAM_CONCURRENCY slots for as long as it lives.
//
// Vars, not consts, so tests can drive both bounds without waiting on them.
var (
	streamIdle = 60 * time.Second
	streamMax  = 30 * time.Minute
)

// errStreamIdle and errStreamDeadline name the two ways a stream deadline gives
// up. They exist so the audit record can tell "the gateway capped this" from
// "the transport failed" — a bare context.Canceled from a timer cannot.
var (
	errStreamIdle     = errors.New("stream idle timeout")
	errStreamDeadline = errors.New("stream duration limit")
)

// streamDeadline bounds an in-flight stream by inactivity, with an overall
// ceiling. The caller touches it after every chunk read; the derived context
// cancels when the gap between touches exceeds idle, when max elapses from
// construction, or when parent is cancelled — a client hanging up, which is
// always the fastest possible release of the upstream slot.
//
// context.WithCancelCause is what keeps those three distinguishable: a plain
// Timer-driven cancel would reach every Recv as an undifferentiated
// context.Canceled, which is exactly the ambiguity that made the old
// whole-stream cap uninterpretable in the logs.
type streamDeadline struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	idle   time.Duration
	max    time.Time // zero when unbounded
}

func newStreamDeadline(parent context.Context, idle, overall time.Duration) *streamDeadline {
	ctx, cancel := context.WithCancelCause(parent)
	d := &streamDeadline{ctx: ctx, cancel: cancel, idle: idle}
	if overall > 0 {
		d.max = time.Now().Add(overall)
	}
	d.timer = time.AfterFunc(d.until(), d.fired)
	return d
}

// fired runs on the timer goroutine. It can only have been armed for the sooner
// of the two deadlines, so the elapsed one is decided by comparing against max.
func (d *streamDeadline) fired() {
	if !d.max.IsZero() && !time.Now().Before(d.max) {
		d.cancel(errStreamDeadline)
		return
	}
	d.cancel(errStreamIdle)
}

// touch records progress, re-arming the timer for the sooner of the idle gap
// and whatever is left of the overall ceiling.
func (d *streamDeadline) touch() { d.timer.Reset(d.until()) }

func (d *streamDeadline) until() time.Duration {
	left := d.idle
	if !d.max.IsZero() {
		if until := time.Until(d.max); until < left {
			left = until
		}
	}
	if left < 0 {
		left = 0
	}
	return left
}

// cause reports why the deadline fired, or nil if it has not. Cancellation from
// parent is deliberately not reported here — a client that hung up is not a
// timeout, and the caller already knows by reading r.Context().
func (d *streamDeadline) cause() error {
	switch cause := context.Cause(d.ctx); {
	case errors.Is(cause, errStreamIdle), errors.Is(cause, errStreamDeadline):
		return cause
	default:
		return nil
	}
}

// stop disarms the timer. The cause of a cancel that already happened survives
// it, so a deferred stop does not erase the reason a stream was capped.
func (d *streamDeadline) stop() {
	d.timer.Stop()
	d.cancel(context.Canceled)
}

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
// shape, the metadata the adapter extracted from that payload (audit text,
// served model, finish reason, token counts — see chunkMeta), and io.EOF once
// the upstream stream ends normally. Close must always be called, and releases
// the underlying response body.
//
// This replaces a ChatStream(w, r, …) method that took an http.ResponseWriter.
// Handing adapters the writer meant SSE framing, error payloads, flushing and
// audit logging were reimplemented per engine type, and the translation could
// not be exercised without standing up an httptest recorder and parsing the
// framing back off. With a reader the handler owns the wire format once and a
// test can drive an adapter's translator straight from a bytes.Reader.
//
// The metadata rides along for the same reason the payload is pre-shaped:
// the adapter has already parsed the chunk (ollamaStream to translate it,
// passthroughStream to read it), so it can hand the handler its audit text
// and usage for free. Having the handler re-unmarshal every frame was a
// second full parse of every token of every completion, on the request path,
// to re-derive text that was in hand one frame earlier.
type ChatStream interface {
	Recv() (payload []byte, meta chunkMeta, err error)
	Close() error
}

// deltaAuditText renders a delta's client-visible text for the audit log:
// the content plus each tool call as "[tool_call name] arguments". OpenAI
// streams a call as a name on its first fragment and argument fragments
// after; Ollama sends both at once. Writing the name when it appears and
// every fragment as it arrives reassembles both shapes.
func deltaAuditText(d Delta) string {
	if len(d.ToolCalls) == 0 {
		return d.Content
	}
	var b strings.Builder
	b.WriteString(d.Content)
	for _, tc := range d.ToolCalls {
		if tc.Function.Name != "" {
			b.WriteString("\n[tool_call " + tc.Function.Name + "] ")
		}
		b.WriteString(tc.Function.Arguments)
	}
	return b.String()
}

// auditChunk is the subset of a chat.completion.chunk the gateway reads for
// its own records: audit text, the served model, the finish reason, and the
// usage block (which OpenAI sends only on the final chunk, if at all).
type auditChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// metaFromChunk extracts the request-log metadata from an upstream chunk that
// arrives as bytes (the passthrough path). A payload that is not a chunk — an
// error object, say — contributes nothing: the stream already relayed it.
func metaFromChunk(payload []byte) chunkMeta {
	var c auditChunk
	if json.Unmarshal(payload, &c) != nil {
		return chunkMeta{}
	}
	var b strings.Builder
	var meta chunkMeta
	meta.model = c.Model
	if c.Usage != nil {
		meta.inTokens, meta.outTokens = c.Usage.PromptTokens, c.Usage.CompletionTokens
	}
	for _, ch := range c.Choices {
		b.WriteString(ch.Delta.Content)
		for _, tc := range ch.Delta.ToolCalls {
			if tc.Function.Name != "" {
				b.WriteString("\n[tool_call " + tc.Function.Name + "] ")
			}
			b.WriteString(tc.Function.Arguments)
		}
		if ch.FinishReason != nil {
			meta.finish = *ch.FinishReason
		}
	}
	meta.auditText = b.String()
	return meta
}

// ollamaStream translates Ollama's newline-delimited JSON into OpenAI chunks.
type ollamaStream struct {
	body         io.ReadCloser
	scanner      *bufio.Scanner
	completionID string
	isFirst      bool
	done         bool

	// toolCalls counts the tool calls already sent. OpenAI clients assemble
	// calls by their index across the whole stream, and the terminal chunk's
	// finish_reason must say "tool_calls" if any were made — while Ollama
	// sends the calls on an earlier line and ends with an ordinary "stop".
	toolCalls int
}

func (s *ollamaStream) Recv() ([]byte, chunkMeta, error) {
	if s.done {
		return nil, chunkMeta{}, io.EOF
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
		c := formatStreamChunk(chunk, s.completionID, s.isFirst)
		choice := &c.Choices[0]
		for i := range choice.Delta.ToolCalls {
			choice.Delta.ToolCalls[i].Index = s.toolCalls
			s.toolCalls++
		}
		if choice.FinishReason != nil && s.toolCalls > 0 {
			choice.FinishReason = &finishToolCalls
		}
		// The metadata comes out of the typed chunk that is already in
		// hand — no second parse, and the tool-call renumbering above is
		// already reflected in the audit text.
		meta := chunkMeta{auditText: deltaAuditText(choice.Delta), model: c.Model}
		if choice.FinishReason != nil {
			meta.finish = *choice.FinishReason
		}
		if c.Usage != nil {
			meta.inTokens, meta.outTokens = c.Usage.PromptTokens, c.Usage.CompletionTokens
		}
		out, err := json.Marshal(c)
		if err != nil {
			return nil, chunkMeta{}, err
		}
		s.isFirst = false
		// Report this chunk, then stop on the next call — the final Ollama
		// object carries both done:true and the usage counts, so returning
		// EOF here instead would discard them.
		if fin, _ := chunk["done"].(bool); fin {
			s.done = true
		}
		return out, meta, nil
	}
	if err := s.scanner.Err(); err != nil {
		return nil, chunkMeta{}, err
	}
	return nil, chunkMeta{}, io.EOF
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

func (s *passthroughStream) Recv() ([]byte, chunkMeta, error) {
	if s.done {
		return nil, chunkMeta{}, io.EOF
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
			return nil, chunkMeta{}, io.EOF
		}
		// One parse serves the audit text, the served model and the usage
		// block; the bytes go to the client unmodified either way.
		return []byte(payload), metaFromChunk([]byte(payload)), nil
	}
	if err := s.scanner.Err(); err != nil {
		return nil, chunkMeta{}, err
	}
	return nil, chunkMeta{}, io.EOF
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
//
// No row yet is the first-run state and comes back as an empty list, not an
// error. Any other error must reach the caller: every engine edit is a
// read-modify-write of this one value, so treating a failed read as "no
// engines" and saving the result replaces the whole list.
func getEngines() ([]EngineConfig, error) {
	var val string
	err := dbRead.QueryRow("SELECT value FROM settings WHERE key=?", "engines").Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
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
	if err := reloadEngineCache(); err != nil {
		return err
	}
	// After the reload, not before: the async warm reads the adapter cache, so
	// a new engine must be in it by the time the warm lists engines. On a
	// failed reload the in-memory set is unchanged, and leaving its probe data
	// cached is exactly right.
	onEnginesChanged()
	return nil
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

	// probeGen counts invalidations, guarded by probeCacheMu. A refresh
	// records it when it starts and stores its result only if it has not
	// moved: a probe that was already in flight when a pull or delete
	// invalidated the cache read the engine as it was before, and storing
	// that would undo the invalidation for another probeCacheTTL.
	probeGen uint64
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
		gen := probeGen
		probeCacheMu.Unlock()
		return refreshProbe(id, e, done, gen)
	}
}

// refreshProbe probes upstream, stores the result, and releases callers
// blocked on done. If the probe panics it clears the entry rather than
// leaving an in-flight marker behind a closed channel, which would spin
// waiters forever.
//
// gen is probeGen as it was when the refresh was published. If the cache has
// been invalidated since, the in-flight marker went with the old map: the
// result is returned to this caller but not stored, and nothing is deleted,
// since any entry now under id belongs to a newer refresh.
func refreshProbe(id int, e Engine, done chan struct{}, gen uint64) probeData {
	finished := false
	defer func() {
		if !finished {
			probeCacheMu.Lock()
			if probeGen == gen {
				delete(probeCache, id)
			}
			probeCacheMu.Unlock()
		}
		close(done)
	}()

	data := probeEngineFull(e)

	probeCacheMu.Lock()
	if probeGen == gen {
		probeCache[id] = probeEntry{
			probeData: data,
			expiry:    time.Now().Add(probeCacheTTL),
		}
	}
	finished = true
	probeCacheMu.Unlock()
	return data
}

// invalidateProbeCache clears the probe cache. Call after engine edits so
// the admin sees fresh data immediately.
func invalidateProbeCache() {
	probeCacheMu.Lock()
	probeCache = map[int]probeEntry{}
	probeGen++
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

// NavEngine is one row of the sidebar's engine box.
type NavEngine struct {
	Name  string
	State string // "online", "offline" or "unknown"
}

// navEngines reports every configured engine's state from the probe cache
// without ever waiting on an upstream: the sidebar is on every admin page, and
// one dead engine must not slow them all down. When the cache has gaps it
// starts a background warm-up, so the next page load shows fresh states.
func navEngines() []NavEngine {
	engines := cachedEngines()
	out := make([]NavEngine, 0, len(engines))
	refresh := false
	for _, e := range engines {
		state, stale := peekProbeState(e.ID)
		refresh = refresh || stale
		out = append(out, NavEngine{Name: e.Name, State: state})
	}
	if refresh {
		go warmProbeCache(context.Background())
	}
	return out
}

// peekProbeData reads one engine's full probe data without probing. found is
// false when there is nothing to show: no entry at all, or the in-flight
// marker of a first probe that has not finished — its zero values would read
// as "down with no models" rather than "not yet known". stale marks data past
// its expiry; a refresh already in flight is not stale, since another warm
// would only queue behind it.
func peekProbeData(id int) (pd probeData, found, stale bool) {
	probeCacheMu.Lock()
	defer probeCacheMu.Unlock()
	entry, ok := probeCache[id]
	if !ok {
		return probeData{}, false, true
	}
	if entry.expiry.IsZero() {
		return probeData{}, false, entry.inflight == nil
	}
	return entry.probeData, true, entry.inflight == nil && !time.Now().Before(entry.expiry)
}

// cachedProbeSnapshot returns probe data for every cached engine that has
// finished at least one probe, and whether any engine needs a refresh. It
// never waits on an upstream: the point is that a page renders from what the
// cache already holds — even expired data — while a refresh runs behind it,
// instead of paying for a probe round on the request path.
func cachedProbeSnapshot() (map[int]probeData, bool) {
	engines := cachedEngines()
	snap := make(map[int]probeData, len(engines))
	stale := false
	for _, e := range engines {
		pd, found, needsRefresh := peekProbeData(e.ID)
		if found {
			snap[e.ID] = pd
		}
		stale = stale || needsRefresh
	}
	return snap, stale
}

// onEnginesChanged is what every engine or model mutation calls: it drops the
// probe cache and the routing index derived from it, then immediately starts
// refilling both off the request path. Invalidation alone made the next reader
// pay — a cold route index is a synchronous rebuild, and a cold probe cache
// plus a peek-less reader is a blocking warm. Kicking the refill here means
// the next page load renders from the previous snapshot instead.
//
// The warm runs detached (no caller context): a pull handler returning must
// not cancel the refresh its own change just triggered.
func onEnginesChanged() {
	invalidateProbeCache()
	go warmProbeCache(context.Background())
	refreshRouteIndexAsync()
}

// peekProbeState reads one engine's state from the probe cache without
// probing. It returns the state to show ("online", "offline" or "unknown")
// and whether the cache needs a refresh for that engine.
//
// An expired entry still shows its last result: a state a few seconds old is
// more useful than "unknown", and the refresh it triggers replaces it. A
// refresh already in flight is not stale; starting another would only queue
// behind it.
func peekProbeState(id int) (state string, stale bool) {
	probeCacheMu.Lock()
	defer probeCacheMu.Unlock()
	entry, ok := probeCache[id]
	if !ok {
		return "unknown", true
	}
	// A zero expiry means no probe has finished yet: the entry is the
	// in-flight marker of the first one, and its zero status is not "down".
	if entry.expiry.IsZero() {
		return "unknown", entry.inflight == nil
	}
	state = "offline"
	if entry.status {
		state = "online"
	}
	return state, entry.inflight == nil && !time.Now().Before(entry.expiry)
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

func (a *OllamaAdapter) baseURL() string { return ollamaEndpoint(a.cfg) }

// ollamaEndpoint is the one place an Ollama engine's scheme://host:port is
// assembled. Every Ollama call — the adapter, the dashboard's endpoint column,
// model deletion — must go through it: building "base:port" by hand elsewhere
// is how a BaseURL that already carried a port became "host:11434:11434".
func ollamaEndpoint(cf EngineConfig) string {
	u := strings.TrimRight(strings.TrimSpace(cf.BaseURL), "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		// Without a scheme net/http rejects the URL outright, so a BaseURL
		// typed as a bare "127.0.0.1" would fail every probe.
		u = "http://" + u
	}
	hostPort := strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
	if _, _, err := net.SplitHostPort(hostPort); err == nil {
		// URL already contains a port — use as-is.
		return u
	}

	port := cf.Port
	if port == 0 {
		port = 11434
	}
	return fmt.Sprintf("%s:%d", u, port)
}

func (a *OllamaAdapter) Status() (bool, error) {
	var up bool
	err := observeUpstream(EngineOllama, "status", func() error {
		resp, err := controlClient.Get(a.baseURL() + "/api/version")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return statusErr(resp, &up)
	})
	return up, err
}

func (a *OllamaAdapter) ListModels() ([]map[string]interface{}, error) {
	var tags map[string]interface{}
	err := observeUpstream(EngineOllama, "list_models", func() error {
		resp, err := controlClient.Get(a.baseURL() + "/api/tags")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("ollama returned %d", resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(&tags)
	})
	if err != nil {
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

	stream, err := openUpstreamStream(EngineOllama, httpReq)
	if err != nil {
		return nil, err
	}
	return &ollamaStream{
		body:         stream,
		scanner:      newStreamScanner(stream),
		completionID: newCompletionID(),
		isFirst:      true,
	}, nil
}

// openUpstreamStream sends a streaming request and, once upstream has accepted
// it, returns the live response body. On success the caller owns the body and
// must Close it.
//
// It is timed as op="stream", and the timing deliberately stops here, at the
// response headers. The full stream lasts as long as the generation does, so
// timing that would measure output length rather than engine latency; the
// whole-request duration is already in gw42_http_request_duration_seconds.
func openUpstreamStream(engineType string, httpReq *http.Request) (io.ReadCloser, error) {
	start := time.Now()
	resp, err := sharedClient.Do(httpReq)
	if err != nil {
		recordUpstream(engineType, "stream", start, err)
		return nil, err
	}
	if resp.StatusCode != 200 {
		// Not deferred because the success path hands this body to the stream
		// and must not close it. The close error is dropped on purpose: we are
		// already returning the failure the caller needs to see, and replacing
		// it with a close error would hide the status code.
		_ = resp.Body.Close()
		err := fmt.Errorf("engine returned %d", resp.StatusCode)
		recordUpstream(engineType, "stream", start, err)
		return nil, err
	}
	recordUpstream(engineType, "stream", start, nil)
	return resp.Body, nil
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

// upstream joins the engine's base URL with an API path such as
// "/v1/models".
//
// The base may be given with or without its /v1: OpenAI-compatible servers
// (oMLX, LM Studio, vLLM) and the OpenAI SDKs all document the base URL as
// "http://host:port/v1", while every path here already starts with /v1. A
// trailing /v1 is therefore dropped from the base, not the path, so a prefix
// in front of it (a proxy mount like /openai/v1) survives. Normalising here
// rather than when the form is saved also repairs engines stored that way.
func (a *OpenAICompatAdapter) upstream(path string) string {
	u := strings.TrimSpace(a.cfg.BaseURL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	u = strings.TrimSuffix(strings.TrimRight(u, "/"), "/v1")
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
	var up bool
	err := observeUpstream(EngineOpenAICompat, "status", func() error {
		resp, err := a.doReq("GET", "/v1/models", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return statusErr(resp, &up)
	})
	return up, err
}

func (a *OpenAICompatAdapter) ListModels() ([]map[string]interface{}, error) {
	var result map[string]interface{}
	err := observeUpstream(EngineOpenAICompat, "list_models", func() error {
		resp, err := a.doReq("GET", "/v1/models", nil)
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

	stream, err := openUpstreamStream(EngineOpenAICompat, httpReq)
	if err != nil {
		return nil, err
	}
	return &passthroughStream{body: stream, scanner: newStreamScanner(stream)}, nil
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

// OllamaDeleteModel deletes a model from an Ollama instance. endpoint is the
// full scheme://host:port, as returned by ollamaEndpoint.
func OllamaDeleteModel(endpoint, model string) error {
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

	// Ollama answers a refused delete with a non-2xx and a reason in the body.
	// Discarding the status made every rejection — unknown model, model in
	// use, read-only store — look like a successful delete to the caller, and
	// the admin UI reported one.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ollama delete %q: %s%s", model, resp.Status, upstreamErrDetail(resp.Body))
	}
	return nil
}

// statusErr turns a health-probe response into the Status result. A non-200
// is an error naming the status and the upstream's reason, not a bare
// "down": the engine was reached and refused, and only the status tells a
// wrong API key (401) from a wrong base URL (404).
func statusErr(resp *http.Response, up *bool) error {
	*up = resp.StatusCode == http.StatusOK
	if *up {
		return nil
	}
	return fmt.Errorf("engine returned %s%s", resp.Status, upstreamErrDetail(resp.Body))
}

// upstreamErrDetail reads a bounded excerpt of an error response body and
// formats it for appending to an error message, or returns "" when the body
// adds nothing the status has not already said.
//
// Bounded twice on purpose. These strings reach the admin through a flash, and
// flashes are serialised into the session *cookie*, so an engine that answers
// an error with a page of HTML would push the cookie past the ~4KB browsers
// accept — turning a readable error into a failed session save.
func upstreamErrDetail(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 2048))
	if err != nil || len(raw) == 0 {
		return ""
	}
	// Ollama puts the reason in a JSON "error" string; OpenAI-compatible
	// engines nest it as error.message. Anything that matches neither is
	// shown as-is, so an unexpected upstream is still diagnosable.
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	msg := string(raw)
	if json.Unmarshal(raw, &payload) == nil && len(payload.Error) > 0 {
		var flat string
		var nested struct {
			Message string `json:"message"`
		}
		switch {
		case json.Unmarshal(payload.Error, &flat) == nil && flat != "":
			msg = flat
		case json.Unmarshal(payload.Error, &nested) == nil && nested.Message != "":
			msg = nested.Message
		}
	}
	// Collapse to a single line: this ends up in a one-line flash.
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		return ""
	}
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	return ": " + msg
}
