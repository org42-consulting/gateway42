package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	_ "modernc.org/sqlite"
)

// ─────────────────────────────── Embedded assets ───────────────────────────────

//go:embed templates
var templateFS embed.FS

//go:embed images
var imageFS embed.FS

// Template data structs

// ─────────────────────────────── Configuration ─────────────────────────────────

// Config holds the application configuration
type Config struct {
	DBPath              string
	AdminPassword       string
	DefaultRL           int
	SessionTTL          int
	MaxMsgLen           int
	LogLevel            string
	LogFile             string
	Port                string
	TLSCert             string
	TLSKey              string
	UpstreamConcurrency int

	// LogRetentionDays bounds how long rows stay in the logs and request_logs
	// tables. Interaction logs hold full prompts and responses (up to
	// MaxMsgLen each), so an unbounded table grows with traffic and holds
	// user content indefinitely. 0 disables pruning.
	LogRetentionDays int
}

var cfg Config

// getEnv retrieves an environment variable or returns a fallback value
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvInt retrieves an integer environment variable or returns a fallback value
func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

// loadConfig loads the application configuration from environment variables
func loadConfig() Config {
	c := Config{
		DBPath:              getEnv("GW42_DB_PATH", "./db/gateway.db"),
		AdminPassword:       getEnv("ADMIN_PASSWORD", ""),
		DefaultRL:           getEnvInt("DEFAULT_RATE_LIMIT", 10),
		SessionTTL:          getEnvInt("SESSION_TIMEOUT", 3600),
		MaxMsgLen:           getEnvInt("MAX_MESSAGE_LENGTH", 262144), // ~256KB
		LogLevel:            getEnv("LOG_LEVEL", "INFO"),
		LogFile:             getEnv("LOG_FILE", "./logs/gateway.log"),
		Port:                getEnv("PORT", "7000"),
		TLSCert:             getEnv("TLS_CERT", ""),
		TLSKey:              getEnv("TLS_KEY", ""),
		UpstreamConcurrency: getEnvInt("GW42_UPSTREAM_CONCURRENCY", 8),
		LogRetentionDays:    getEnvInt("GW42_LOG_RETENTION_DAYS", 30),
	}
	return c
}

// ─────────────────────────────── Types ─────────────────────────────────────────

// User represents a user in the system
type User struct {
	ID        int
	Name      string
	APIKey    string
	Status    string
	RateLimit int
	CreatedAt string
}

// LogRow represents a log entry for system logs
type LogRow struct {
	ID       int
	Name     string
	Model    string
	Prompt   string
	Response string
	TS       string
}

// UserLogRow represents a log entry for user-specific logs
type UserLogRow struct {
	ID       int
	Model    string
	Prompt   string
	Response string
	TS       string
}

// ModelDetail holds information about a model
type ModelDetail struct {
	Name string
	Size string
}

// FlashMsg represents a flash message for UI notifications
type FlashMsg struct {
	Category string `json:"c"`
	Message  string `json:"m"`
}

// SysLogEntry represents an entry in the system log
type SysLogEntry struct {
	TS    string `json:"ts"`
	Level string `json:"level"`
	Name  string `json:"name"`
	Msg   string `json:"msg"`
}

// RequestLogRow represents a row in the HTTP request log
type RequestLogRow struct {
	ID         int    `json:"ID"`
	TS         string `json:"TS"`
	Method     string `json:"Method"`
	Path       string `json:"Path"`
	ClientIP   string `json:"ClientIP"`
	UserName   string `json:"Name"`
	StatusCode int    `json:"Status"`
}

// ─── Template data structs ────────────────────────────────────────────────────

// BaseData holds common data for all templates
type BaseData struct {
	Flashes     []FlashMsg
	CurrentPath string
	CSRFToken   string
}

// LoginData holds data for the standalone login page. It carries BaseData for
// the CSRF token alone — login.html does not extend base.html, so CurrentPath
// and the nav it drives go unused there.
type LoginData struct {
	BaseData
}

// DashboardData holds data for the dashboard page
type DashboardData struct {
	BaseData
	Users        []User
	Engines      []EngineConfig
	EngineURLs   map[string]string   // engineID → "host:port"
	EngineStatus map[string]bool     // engineID → reachable
	EngineModels map[string][]string // engineID → model names
}

// SettingsData holds data for the settings page
type SettingsData struct {
	BaseData
	Engines            []EngineConfig
	EngineURLs         map[string]string
	EngineStatus       map[string]bool
	EngineModelDetails map[string][]ModelDetail // engineID → model details
	SearchResults      []ModelDetail
}

// LogsData holds data for the logs page
type LogsData struct {
	BaseData
	Search string
}

// HelpData holds data for the help page
type HelpData struct {
	BaseData
}

// ConfirmDeleteData holds data for the delete confirmation page
type ConfirmDeleteData struct {
	BaseData
	UID int
}

// ─────────────────────────────── Global vars ───────────────────────────────────

// Global variables
//
// db     — writer pool, single connection. All Exec/INSERT/UPDATE/DELETE and
//
//	tx.Begin() that mutates state must go through this handle.
//
// dbRead — reader pool, multi-connection, opened with mode=ro. All Query/
//
//	QueryRow that does not need write semantics goes here. Attempting
//	to write via dbRead returns SQLITE_READONLY.
var (
	db           *sql.DB
	dbRead       *sql.DB
	sessionStore *sessions.CookieStore
	syslogBuf    *SyslogBuffer
	tmpls        map[string]*template.Template
)

// ─────────────────────────────── Syslog buffer ────────────────────────────────

const maxSyslogEntries = 500

type SyslogBuffer struct {
	mu  sync.RWMutex
	buf []SysLogEntry
}

func newSyslogBuffer() *SyslogBuffer {
	return &SyslogBuffer{buf: make([]SysLogEntry, 0, maxSyslogEntries)}
}

func (s *SyslogBuffer) Add(e SysLogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) >= maxSyslogEntries {
		s.buf = s.buf[1:]
	}
	s.buf = append(s.buf, e)
}

func (s *SyslogBuffer) Entries() []SysLogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]SysLogEntry, len(s.buf))
	copy(cp, s.buf)
	return cp
}

func (s *SyslogBuffer) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = s.buf[:0]
}

// bufSlogHandler is a slog.Handler that feeds records into SyslogBuffer.
type bufSlogHandler struct {
	buf   *SyslogBuffer
	level slog.Level
}

func (h *bufSlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *bufSlogHandler) Handle(_ context.Context, r slog.Record) error {
	h.buf.Add(SysLogEntry{
		TS:    r.Time.UTC().Format("2006-01-02T15:04:05"),
		Level: r.Level.String(),
		Name:  "gateway42",
		Msg:   r.Message,
	})
	return nil
}

func (h *bufSlogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *bufSlogHandler) WithGroup(_ string) slog.Handler      { return h }

// multiSlogHandler fans out to two slog.Handlers.
type multiSlogHandler struct{ a, b slog.Handler }

func (m *multiSlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return m.a.Enabled(ctx, level) || m.b.Enabled(ctx, level)
}
func (m *multiSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if m.a.Enabled(ctx, r.Level) {
		_ = m.a.Handle(ctx, r)
	}
	if m.b.Enabled(ctx, r.Level) {
		_ = m.b.Handle(ctx, r)
	}
	return nil
}
func (m *multiSlogHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &multiSlogHandler{m.a.WithAttrs(a), m.b.WithAttrs(a)}
}
func (m *multiSlogHandler) WithGroup(name string) slog.Handler {
	return &multiSlogHandler{m.a.WithGroup(name), m.b.WithGroup(name)}
}

// DB, user/admin queries, rate limiter, and auth helpers live in db.go and auth.go.

// ─────────────────────────────── Session / flash ───────────────────────────────

const sessionName = "gw42_session"

func getSession(r *http.Request) *sessions.Session {
	sess, _ := sessionStore.Get(r, sessionName)
	return sess
}

func isAdminSession(r *http.Request) bool {
	sess := getSession(r)
	v, _ := sess.Values["admin"].(bool)
	return v
}

// newBaseData assembles the fields every rendered page needs. Going through one
// constructor is what guarantees a CSRF token reaches every template — a page
// that built BaseData by hand would render forms that the middleware then
// rejects. It consumes flashes, so call it exactly once per response.
func newBaseData(w http.ResponseWriter, r *http.Request) BaseData {
	return BaseData{
		Flashes:     consumeFlashes(w, r, getSession(r)),
		CurrentPath: r.URL.Path,
		CSRFToken:   csrfToken(w, r),
	}
}

func addFlash(w http.ResponseWriter, r *http.Request, category, message string) {
	sess := getSession(r)
	var flashes []FlashMsg
	if raw, ok := sess.Values["flashes"].(string); ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &flashes); err != nil {
			// A half-decoded slice would get the new message appended to it and
			// re-encoded, making the corruption permanent. Drop the queue and
			// keep the message the caller actually cares about.
			slog.Warn("discarding unreadable flash queue", "err", err)
			flashes = nil
		}
	}
	flashes = append(flashes, FlashMsg{category, message})
	b, _ := json.Marshal(flashes)
	sess.Values["flashes"] = string(b)
	_ = saveSession(w, r, sess)
}

func consumeFlashes(w http.ResponseWriter, r *http.Request, sess *sessions.Session) []FlashMsg {
	raw, ok := sess.Values["flashes"].(string)
	if !ok || raw == "" {
		return nil
	}
	var flashes []FlashMsg
	if err := json.Unmarshal([]byte(raw), &flashes); err != nil {
		slog.Warn("discarding unreadable flash queue", "err", err)
		flashes = nil
	}
	// Clear the queue even when the decode failed, so a corrupt value cannot
	// re-log a warning on every subsequent page render.
	sess.Values["flashes"] = ""
	_ = saveSession(w, r, sess)
	return flashes
}

// ── Engine helpers ─────────────────────────────────────────────────────────────

// getEngineURLs returns a map of engineID → "host:port" for all configured engines.
func getEngineURLs(engines []EngineConfig) map[string]string {
	urls := make(map[string]string)
	for _, e := range engines {
		urls[fmt.Sprintf("%d", e.ID)] = engineEndpoint(e)
	}
	return urls
}

// engineEndpoint returns the full endpoint for an engine config.
func engineEndpoint(cfg EngineConfig) string {
	host := cfg.BaseURL
	if cfg.Type == EngineOllama {
		u := strings.TrimRight(host, "/")
		port := cfg.Port
		if port == 0 {
			port = 11434
		}
		return fmt.Sprintf("%s:%d", u, port)
	}
	// OpenAI-compatible: include port if non-standard
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "http://" + host
	}
	return strings.TrimRight(host, "/")
}

// ─────────────────────────────── Templates ────────────────────────────────────

func initTemplates() {
	funcMap := template.FuncMap{
		"hasPrefix": strings.HasPrefix,
	}

	tmpls = make(map[string]*template.Template)
	base := "templates/base.html"

	for _, page := range []string{"dashboard", "settings", "logs", "help", "confirm_delete"} {
		t := template.Must(
			template.New("").Funcs(funcMap).ParseFS(templateFS, base, "templates/"+page+".html"),
		)
		tmpls[page] = t
	}
	// login is standalone
	tmpls["login"] = template.Must(
		template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/login.html"),
	)
}

func renderPage(w http.ResponseWriter, page string, data interface{}) {
	t, ok := tmpls[page]
	if !ok {
		http.Error(w, "template not found: "+page, 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var name string
	if page == "login" {
		name = "login.html"
	} else {
		name = "base.html"
	}
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("template error", "page", page, "err", err)
	}
}

// ─────────────────────────────── Background tasks ─────────────────────────────

func startBackgroundTasks() {
	// One-shot: drain the legacy DB-backed rate-limit table left over from
	// prior versions. The new limiter is in-memory.
	truncateLegacyRateLimitTable()

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			sweepIdleLimiters()
		}
	}()

	if cfg.LogRetentionDays > 0 {
		go func() {
			// Prune once at startup so a gateway that is restarted more often
			// than the interval still enforces retention, then hourly. Hourly
			// rather than daily keeps each sweep small: a day's backlog would
			// exceed the batch cap and spill to the next run anyway.
			runLogPrune()
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				runLogPrune()
			}
		}()
	}
}

// runLogPrune executes one retention sweep and logs the outcome. Failure is
// not fatal: the next tick retries, and serving traffic matters more than
// reclaiming disk.
func runLogPrune() {
	logs, requests, err := pruneLogs()
	if err != nil {
		slog.Error("log prune", "err", err)
		return
	}
	if logs > 0 || requests > 0 {
		slog.Info("log prune", "interaction_rows", logs, "request_rows", requests,
			"retention_days", cfg.LogRetentionDays)
	}
}

// ─────────────────────────────── CORS ─────────────────────────────────────────

var corsHeaders = map[string]string{
	"Access-Control-Allow-Origin":  "*",
	"Access-Control-Allow-Methods": "GET, POST, OPTIONS",
	"Access-Control-Allow-Headers": "Authorization, Content-Type",
	"Access-Control-Max-Age":       "86400",
}

// ─────────────────────────────── Request logging ──────────────────────────────

// statusRecorder wraps an http.ResponseWriter to capture the status code for
// metrics and request logging, and to record whether the response has been
// committed. The commit flag is what lets recoveryMiddleware tell a panic it
// can still answer with a status code from one it can only append to.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w, status: 200}
}

func (r *statusRecorder) WriteHeader(code int) {
	// First call wins, matching net/http: a second WriteHeader is ignored there
	// with a warning, so recording the later code would misreport the response.
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write marks the response committed. net/http sends an implicit 200 on the
// first write, so a handler that never calls WriteHeader — a rendered template,
// say — still leaves a panic handler nothing it can amend.
func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ensureRecorder returns the statusRecorder already in play, or installs one.
//
// Three middlewares need the status: metrics, recovery and the request log.
// Wrapping once each stacked three recorders per request, and only the
// innermost saw the handler's own WriteHeader — the outer two recorded whatever
// their child had passed through, which happened to agree only because the
// wrapper is transparent. Sharing one makes the agreement structural instead of
// accidental, and it is what lets metrics read the status recovery wrote.
//
// A writer that is not a recorder is wrapped, so calling any of these
// middlewares standalone (as the tests do) still works.
func ensureRecorder(w http.ResponseWriter) *statusRecorder {
	if rec, ok := w.(*statusRecorder); ok {
		return rec
	}
	return newStatusRecorder(w)
}

// panicStatus reports the status the client will actually observe for a
// response that is unwinding a panic. writePanicResponse can only supply a
// status while nothing has been written; once the response is committed the
// status on the wire is already fixed and a panic can at most append to the
// body. Both the response and the request log read this, so the two cannot
// disagree about what the caller saw.
func panicStatus(rec *statusRecorder) int {
	if rec.wrote {
		return rec.status
	}
	return http.StatusInternalServerError
}

// userCtxKey is the context key under which the authenticated User is stored
// by apiAuthMiddleware. Handlers retrieve via userFromContext.
type userCtxKey struct{}

func userFromContext(r *http.Request) *User {
	u, _ := r.Context().Value(userCtxKey{}).(*User)
	return u
}

// apiAuthMiddleware resolves the Bearer-token user once for /v1/* routes and
// stores it in the request context. Downstream handlers and the request
// logger read it back from context instead of re-resolving — halving DB/cache
// lookups on the hot path.
func apiAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			jsonResponse(w, http.StatusUnauthorized,
				openaiError("Missing or malformed Bearer token", "authentication_error"))
			return
		}
		u, err := getUserByAPIKey(auth[7:])
		if err != nil || u == nil || u.Status != "active" {
			jsonResponse(w, http.StatusUnauthorized,
				openaiError("Invalid API key", "authentication_error"))
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey{}, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}

		rec := ensureRecorder(w)

		// Deferred, so a panic below still produces an audit row. This layer sits
		// inside recoveryMiddleware, so at this point the 500 has not been written
		// yet — panicStatus supplies the status recovery is about to present,
		// rather than the 200 the recorder still holds.
		panicking := true
		defer func() {
			status := rec.status
			if panicking {
				status = panicStatus(rec)
			}

			var userName string
			if u := userFromContext(r); u != nil {
				userName = u.Name
			}

			// Non-blocking: enqueueRequestLog drops on overflow rather than
			// spawning unbounded goroutines.
			insertRequestLog(r.Method, r.URL.Path, clientIP(r), userName, status)
		}()

		next.ServeHTTP(rec, r)
		panicking = false
	})
}

// clientIP extracts the caller's address for logging. X-Forwarded-For wins when
// present, so a reverse-proxied deployment logs the real client rather than the
// proxy — which also means it is spoofable when Gateway42 is exposed directly.
// Treat the value as log metadata only, never as an authorization input.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		return strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ─────────────────────────────── Router ───────────────────────────────────────

func setupRouter() *mux.Router {
	r := mux.NewRouter()

	// Static images
	imgSub, _ := fs.Sub(imageFS, "images")
	r.PathPrefix("/images/").Handler(
		http.StripPrefix("/images/", http.FileServer(http.FS(imgSub))),
	)

	// CORS preflight
	r.PathPrefix("/v1/").Methods("OPTIONS").HandlerFunc(handleCorsPreflight)

	r.Use(middlewareChain()...)

	// Public
	r.HandleFunc("/", handleIndex).Methods("GET")
	r.HandleFunc("/health", handleHealth).Methods("GET")
	r.HandleFunc("/admin", handleAdminLogin).Methods("POST")
	r.HandleFunc("/logout", handleLogout).Methods("GET")

	// Admin UI
	r.HandleFunc("/admin/panel", handleAdminPanel).Methods("GET")
	r.HandleFunc("/admin/settings-page", handleAdminSettingsPage).Methods("GET")
	r.HandleFunc("/admin/engine-test", handleEngineTest).Methods("GET")
	r.HandleFunc("/admin/ollama-pull-stream", handleOllamaPullStream).Methods("GET")
	r.HandleFunc("/admin/ollama-pull-search-stream", handleOllamaPullSearchStream).Methods("GET")
	r.HandleFunc("/admin/ollama-delete-model", handleOllamaDeleteModel).Methods("POST")
	r.HandleFunc("/admin/engine-settings", handleEngineSettings).Methods("POST")
	r.HandleFunc("/admin/engine-remove", handleRemoveEngine).Methods("POST")
	r.HandleFunc("/admin/ollama-search", handleOllamaSearch).Methods("GET")
	r.HandleFunc("/admin/change-password", handleChangePassword).Methods("POST")
	r.HandleFunc("/admin/help", handleAdminHelp).Methods("GET")
	r.HandleFunc("/admin/logs", handleAdminLogs).Methods("GET")
	r.HandleFunc("/admin/logs/data", handleAdminLogsData).Methods("GET")
	r.HandleFunc("/admin/logs/system", handleAdminSystemLogs).Methods("GET")
	r.HandleFunc("/admin/logs/system/export", handleAdminSystemLogsExport).Methods("GET")
	r.HandleFunc("/admin/logs/system/reset", handleAdminSystemLogsReset).Methods("POST")
	r.HandleFunc("/admin/export-logs", handleExportAllLogs).Methods("GET")
	r.HandleFunc("/admin/reset-system", handleResetSystem).Methods("POST")
	r.HandleFunc("/admin/reset/{uid:[0-9]+}", handleAdminResetKey).Methods("POST")
	r.HandleFunc("/admin/update-rate-limit/{uid:[0-9]+}", handleUpdateRateLimit).Methods("POST")

	r.HandleFunc("/register", handleRegister).Methods("POST")
	r.HandleFunc("/toggle/{uid:[0-9]+}", handleToggle).Methods("POST")
	r.HandleFunc("/export/{uid:[0-9]+}", handleExport).Methods("GET")
	r.HandleFunc("/confirm-delete/{uid:[0-9]+}", handleConfirmDelete).Methods("GET")
	r.HandleFunc("/delete/{uid:[0-9]+}", handleDelete).Methods("POST")
	r.HandleFunc("/users", handleUsers).Methods("GET")

	// OpenAI-compatible API
	r.HandleFunc("/v1/models", handleListModels).Methods("GET")
	r.HandleFunc("/v1/chat/completions", handleChatCompletions).Methods("POST")

	// Prometheus metrics (admin session required).
	r.Handle("/metrics", handleMetrics()).Methods("GET")

	return r
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			for k, v := range corsHeaders {
				w.Header().Set(k, v)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// middlewareChain is the middleware stack applied to every route, outermost
// first. It is a function rather than inline r.Use calls so that tests can drive
// requests through the real order instead of a hand-rebuilt copy of it —
// the ordering here is load-bearing and its failure mode is silent.
//
// Order matters throughout, and the first two are ordered the way they are for
// a reason that is easy to get backwards. Middleware defers unwind inside-out,
// so the layer that writes the panic response has to sit inside the layer that
// measures it. With recovery outermost — the intuitive arrangement — the panic
// unwound through metricsMiddleware before recovery ever saw it and took that
// layer's recording with it, so a recovered panic was missing from the request
// count and the latency histogram entirely. Deferring the recording on its own
// does not fix that, it only changes the symptom: metrics' defer would then run
// before recovery's and read a status nobody had written yet. Metrics outermost
// and recovery second is what makes the two agree.
//
// The cost is that a panic in metricsMiddleware itself is no longer caught here
// and falls through to net/http's per-connection recovery, which drops that one
// connection and logs it. That layer is a dozen lines of Prometheus calls with
// label counts fixed at compile time, so the exposure is small and the process
// still survives; keep it that way.
//
// Below those: cors, then auth (so logging can read the user from context), then
// CSRF (which must reject before any handler mutates state), then logging.
func middlewareChain() []mux.MiddlewareFunc {
	return []mux.MiddlewareFunc{
		metricsMiddleware,
		recoveryMiddleware,
		corsMiddleware,
		apiAuthMiddleware,
		csrfMiddleware,
		requestLoggingMiddleware,
	}
}

// recoveryMiddleware turns a panic in any handler below it into a response
// instead of an abruptly closed connection. It runs inside metricsMiddleware so
// that the status it writes is on the recorder before the metrics are read; see
// the ordering comment on the middleware chain in setupRoutes.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := ensureRecorder(w)
		defer func() {
			if err := recover(); err != nil {
				// Counted separately because the status metric cannot carry this:
				// a panic mid-stream is honestly recorded as the 200 the client
				// received, so without its own counter it is invisible to metrics.
				metricPanics.WithLabelValues(pathBucket(r.URL.Path)).Inc()

				slog.Error("panic recovered", "err", err, "path", r.URL.Path,
					"stack", string(debug.Stack()))
				writePanicResponse(rec, r)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// writePanicResponse reports a recovered panic in a form the client can
// actually parse. The response's own state picks the shape, because a panic can
// land at three different points:
//
//   - Nothing written yet. A real status code is still available, so a /v1/
//     caller gets the same OpenAI error envelope it parses every other error
//     with, and the admin UI gets plain text as before.
//   - Mid-stream. The 200 and the text/event-stream headers are already out and
//     the status cannot change, so an error frame followed by [DONE] is the only
//     way to tell an accumulating client that its completion is truncated. This
//     is deliberately the same shape streamCompletion emits for an upstream read
//     failure, so a client needs one code path for both.
//   - Committed as something else — a half-rendered admin page, say. Anything
//     written now would append garbage to it, so the log entry above is the only
//     report. Attempting a status here is what produced net/http's "superfluous
//     WriteHeader call" noise before.
func writePanicResponse(rec *statusRecorder, r *http.Request) {
	switch {
	case !rec.wrote:
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			jsonResponse(rec, http.StatusInternalServerError,
				openaiError("Internal server error", "api_error"))
			return
		}
		http.Error(rec, "Internal server error", http.StatusInternalServerError)

	case strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream"):
		if b, err := json.Marshal(openaiError("Internal server error", "api_error")); err == nil {
			fmt.Fprintf(rec, "data: %s\n\n", b)
		}
		fmt.Fprint(rec, "data: [DONE]\n\n")
		rec.Flush()
	}
}

// ─────────────────────────────── Main ─────────────────────────────────────────

func main() {
	cfg = loadConfig()

	// Logging
	syslogBuf = newSyslogBuffer()
	logLevel := slog.LevelInfo
	if strings.ToUpper(cfg.LogLevel) == "DEBUG" {
		logLevel = slog.LevelDebug
	}

	textH := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	bufH := &bufSlogHandler{buf: syslogBuf, level: logLevel}
	slog.SetDefault(slog.New(&multiSlogHandler{a: textH, b: bufH}))

	// Session store (random key, sessions invalidated on restart)
	key := make([]byte, 32)
	rand.Read(key)
	sessionStore = sessions.NewCookieStore(key)
	sessionStore.Options = &sessions.Options{
		MaxAge:   cfg.SessionTTL,
		HttpOnly: true,
		Secure:   cfg.TLSCert != "" && cfg.TLSKey != "",
		Path:     "/",
		// Lax, set explicitly: the zero value of http.SameSite omits the
		// attribute entirely, which leaves the behaviour up to each browser's
		// default. Lax stops the cookie riding along on cross-site POSTs,
		// which is the CSRF vector, while still surviving ordinary top-level
		// navigation to the admin panel.
		SameSite: http.SameSiteLaxMode,
	}

	// DB
	if err := initDB(); err != nil {
		slog.Error("Database initialization failed", "err", err)
		os.Exit(1)
	}

	// Engine cache (engines + their HTTP adapters). It is fine for this to
	// return an error: it just means no engines are configured yet.
	if err := reloadEngineCache(); err != nil {
		slog.Info("Engine cache empty on startup", "err", err)
	}

	// Templates
	initTemplates()

	// Background tasks
	startBackgroundTasks()

	// Log writers (batched, async). Cancelling the context drains the
	// in-flight buffers and exits cleanly.
	writersCtx, writersCancel := context.WithCancel(context.Background())
	waitWriters := startLogWriters(writersCtx)

	// Router
	router := setupRouter()

	addr := "0.0.0.0:" + cfg.Port
	srv := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
		// No WriteTimeout: SSE streaming responses can legitimately exceed any fixed bound.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			slog.Info("Gateway42 listening (HTTPS)", "addr", addr, "cert", cfg.TLSCert)
			serverErr <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			slog.Info("Gateway42 listening (HTTP)", "addr", addr)
			serverErr <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		slog.Info("Shutdown signal received, draining connections...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "err", err)
		}
		// Stop log writers and wait for them to drain in-flight batches.
		writersCancel()
		waitWriters()
		// Not deferred: these run only on the clean-shutdown path, after the
		// writers have drained, and the errors are the last chance to learn
		// that the WAL did not checkpoint cleanly — which is exactly the
		// state the zero-byte-WAL recovery in initDB exists to clean up after.
		if dbRead != nil {
			if err := dbRead.Close(); err != nil {
				slog.Error("closing reader pool", "err", err)
			}
		}
		if db != nil {
			if err := db.Close(); err != nil {
				slog.Error("closing writer pool", "err", err)
			}
		}
		slog.Info("Shutdown complete")
	}
}
