package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	gocrypto "golang.org/x/crypto/pbkdf2"
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
	DBPath        string
	OllamaURL     string
	AdminPassword string
	DefaultRL     int
	SessionTTL    int
	MaxMsgLen     int
	LogLevel      string
	LogFile       string
	Port          string
	TLSCert       string
	TLSKey        string
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
		DBPath:        getEnv("GW42_DB_PATH", "./db/gateway.db"),
		OllamaURL:     getEnv("OLLAMA_URL", "http://127.0.0.1:11434/api/chat"),
		AdminPassword: getEnv("ADMIN_PASSWORD", ""),
		DefaultRL:     getEnvInt("DEFAULT_RATE_LIMIT", 10),
		SessionTTL:    getEnvInt("SESSION_TIMEOUT", 3600),
		MaxMsgLen:     getEnvInt("MAX_MESSAGE_LENGTH", 10000),
		LogLevel:      getEnv("LOG_LEVEL", "INFO"),
		LogFile:       getEnv("LOG_FILE", "./logs/gateway.log"),
		Port:          getEnv("PORT", "7000"),
		TLSCert:       getEnv("TLS_CERT", ""),
		TLSKey:        getEnv("TLS_KEY", ""),
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

// ─── Template data structs ────────────────────────────────────────────────────

// BaseData holds common data for all templates
type BaseData struct {
	Flashes     []FlashMsg
	CurrentPath string
}

// DashboardData holds data for the dashboard page
type DashboardData struct {
	BaseData
	Users        []User
	OllamaURL    string
	OllamaPort   int
	OllamaStatus bool
	OllamaModels []string
}

// SettingsData holds data for the settings page
type SettingsData struct {
	BaseData
	OllamaURL          string
	OllamaPort         int
	OllamaStatus       bool
	OllamaModels       []string
	OllamaModelDetails []ModelDetail
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
var (
	db           *sql.DB
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

// ─────────────────────────────── Database ──────────────────────────────────────

// initDB initializes the database connection and creates the schema
// This function also handles cleanup of stale WAL/SHM files that could cause SQLite errors
func initDB() error {
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0755); err != nil {
		return err
	}
	// Remove zero-byte WAL/SHM files left by a previous crash before any data
	// was written; a 0-byte WAL causes SQLITE_IOERR_SHORT_READ (522).
	// Non-zero WAL files contain committed data and must be left for SQLite to recover.
	if fi, err := os.Stat(cfg.DBPath + "-wal"); err == nil && fi.Size() == 0 {
		os.Remove(cfg.DBPath + "-wal")
		os.Remove(cfg.DBPath + "-shm")
	}
	var err error
	dsn := cfg.DBPath + "?_pragma=journal_mode%3DWAL&_pragma=busy_timeout%3D5000"
	db, err = sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	// SQLite requires a single connection; multiple concurrent connections
	// cause SQLITE_IOERR_SHORT_READ (522) and similar I/O errors.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	return createSchema()
}

func createSchema() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users(
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT UNIQUE NOT NULL,
			api_key    TEXT UNIQUE NOT NULL,
			status     TEXT NOT NULL DEFAULT 'pending',
			rate_limit INTEGER NOT NULL DEFAULT 10,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS admin_user(
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			email         TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at    TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS logs(
			id       INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id  INTEGER NOT NULL,
			model    TEXT,
			prompt   TEXT,
			response TEXT,
			ts       TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS settings(
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS rate_limit_entries(
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id   INTEGER NOT NULL,
			timestamp REAL NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_user_id ON logs(user_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_api_key ON users(api_key)`,
		`CREATE INDEX IF NOT EXISTS idx_rate_limit_user_timestamp ON rate_limit_entries(user_id, timestamp)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
	}

	// Migration: rename users.email to users.name
	urows, err := tx.Query("PRAGMA table_info(users)")
	if err != nil {
		return err
	}
	hasNameCol := false
	for urows.Next() {
		var cid int
		var colname, typ string
		var notNull, pk int
		var dflt sql.NullString
		urows.Scan(&cid, &colname, &typ, &notNull, &dflt, &pk)
		if colname == "name" {
			hasNameCol = true
		}
	}
	urows.Close()
	if !hasNameCol {
		if _, err := tx.Exec("ALTER TABLE users RENAME COLUMN email TO name"); err != nil {
			return err
		}
	}

	// Migration: add model column to existing logs tables
	rows, err := tx.Query("PRAGMA table_info(logs)")
	if err != nil {
		return err
	}
	hasModel := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk)
		if name == "model" {
			hasModel = true
		}
	}
	rows.Close()
	if !hasModel {
		if _, err := tx.Exec("ALTER TABLE logs ADD COLUMN model TEXT"); err != nil {
			return err
		}
	}

	// Seed admin user only if none exists yet
	var exists int
	tx.QueryRow("SELECT COUNT(*) FROM admin_user").Scan(&exists)
	if exists == 0 {
		pw := cfg.AdminPassword
		if pw == "" {
			pw = "admin123"
		}
		hash := hashPassword(pw)
		if _, err := tx.Exec(
			"INSERT INTO admin_user(email, password_hash, created_at) VALUES(?,?,?)",
			"admin", hash, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("Database initialized")
	return nil
}

// ── User queries ──────────────────────────────────────────────────────────────

var (
	userCache    = map[interface{}]*User{}
	userCacheMu  sync.RWMutex
	userCacheTTL = 5 * time.Minute
	userCacheExp = map[interface{}]time.Time{}
)

func getUserByAPIKey(apiKey string) (*User, error) {
	key := "api:" + apiKey
	if u := userCacheGet(key); u != nil {
		return u, nil
	}
	row := db.QueryRow("SELECT id, name, api_key, status, rate_limit, created_at FROM users WHERE api_key=?", apiKey)
	u, err := scanUser(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	userCacheSet(key, u)
	return u, nil
}

func getUserByID(id int) (*User, error) {
	key := fmt.Sprintf("id:%d", id)
	if u := userCacheGet(key); u != nil {
		return u, nil
	}
	row := db.QueryRow("SELECT id, name, api_key, status, rate_limit, created_at FROM users WHERE id=?", id)
	u, err := scanUser(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	userCacheSet(key, u)
	return u, nil
}

func getUserByName(name string) (*User, error) {
	row := db.QueryRow("SELECT id, name, api_key, status, rate_limit, created_at FROM users WHERE name=?", name)
	u, err := scanUser(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.APIKey, &u.Status, &u.RateLimit, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func userCacheGet(key interface{}) *User {
	userCacheMu.RLock()
	defer userCacheMu.RUnlock()
	if exp, ok := userCacheExp[key]; ok && time.Now().Before(exp) {
		return userCache[key]
	}
	return nil
}

func userCacheSet(key interface{}, u *User) {
	userCacheMu.Lock()
	defer userCacheMu.Unlock()
	userCache[key] = u
	userCacheExp[key] = time.Now().Add(userCacheTTL)
}

func invalidateUserCache(userID int) {
	userCacheMu.Lock()
	defer userCacheMu.Unlock()
	// Delete the id: entry explicitly.
	idKey := fmt.Sprintf("id:%d", userID)
	delete(userCache, idKey)
	delete(userCacheExp, idKey)
	// Find and delete any api: entry that belongs to this user.
	for k, u := range userCache {
		if s, ok := k.(string); ok && strings.HasPrefix(s, "api:") && u.ID == userID {
			delete(userCache, s)
			delete(userCacheExp, s)
		}
	}
}

func getAllUsers() ([]User, error) {
	rows, err := db.Query("SELECT id, name, api_key, status, rate_limit, created_at FROM users ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		rows.Scan(&u.ID, &u.Name, &u.APIKey, &u.Status, &u.RateLimit, &u.CreatedAt)
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return users, nil
}

func createUser(name, apiKey string, rateLimit int) error {
	_, err := db.Exec(
		"INSERT INTO users(name, api_key, status, rate_limit, created_at) VALUES(?,?,?,?,?)",
		name, apiKey, "disabled", rateLimit, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("user already exists")
		}
		return err
	}
	slog.Info("User created", "name", name)
	return nil
}

func updateUserStatus(userID int, status string) error {
	_, err := db.Exec("UPDATE users SET status=? WHERE id=?", status, userID)
	if err == nil {
		invalidateUserCache(userID)
	}
	return err
}

func resetUserAPIKey(userID int, newKey string) error {
	_, err := db.Exec("UPDATE users SET api_key=? WHERE id=?", newKey, userID)
	if err == nil {
		invalidateUserCache(userID)
	}
	return err
}

func updateUserRateLimit(userID, rateLimit int) error {
	_, err := db.Exec("UPDATE users SET rate_limit=? WHERE id=?", rateLimit, userID)
	if err == nil {
		invalidateUserCache(userID)
	}
	return err
}

func deleteUser(userID int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tx.Exec("DELETE FROM logs WHERE user_id=?", userID)
	tx.Exec("DELETE FROM users WHERE id=?", userID)
	if err := tx.Commit(); err != nil {
		return err
	}
	invalidateUserCache(userID)
	return nil
}

// ── Log queries ───────────────────────────────────────────────────────────────

func logInteraction(userID int, prompt, response, model string) {
	_, err := db.Exec(
		"INSERT INTO logs(user_id, model, prompt, response, ts) VALUES(?,?,?,?,?)",
		userID, model, truncateInput(prompt), truncateInput(response),
		time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		slog.Error("log_interaction failed", "err", err)
	}
}

func getUserLogs(userID int) ([]UserLogRow, error) {
	rows, err := db.Query(
		"SELECT id, model, prompt, response, ts FROM logs WHERE user_id=? ORDER BY ts ASC", userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserLogRow
	for rows.Next() {
		var r UserLogRow
		var model sql.NullString
		rows.Scan(&r.ID, &model, &r.Prompt, &r.Response, &r.TS)
		r.Model = model.String
		out = append(out, r)
	}
	return out, nil
}

func getLogs(search string, limit int) ([]LogRow, error) {
	var (
		sqlRows *sql.Rows
		err     error
	)
	if search != "" {
		p := "%" + search + "%"
		sqlRows, err = db.Query(
			`SELECT l.id, u.name, COALESCE(l.model,''), l.prompt, l.response, l.ts
			FROM logs l JOIN users u ON u.id=l.user_id
			WHERE l.prompt LIKE ? OR l.response LIKE ? OR u.name LIKE ?
			ORDER BY l.ts DESC LIMIT ?`,
			p, p, p, limit,
		)
	} else {
		sqlRows, err = db.Query(
			`SELECT l.id, u.name, COALESCE(l.model,''), l.prompt, l.response, l.ts
			FROM logs l JOIN users u ON u.id=l.user_id
			ORDER BY l.ts DESC LIMIT ?`,
			limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()
	var out []LogRow
	for sqlRows.Next() {
		var r LogRow
		sqlRows.Scan(&r.ID, &r.Name, &r.Model, &r.Prompt, &r.Response, &r.TS)
		out = append(out, r)
	}
	return out, nil
}

func getLogsCount(search string) (int, error) {
	var count int
	var err error
	if search != "" {
		p := "%" + search + "%"
		err = db.QueryRow(
			`SELECT COUNT(*) FROM logs l JOIN users u ON u.id=l.user_id
			WHERE l.prompt LIKE ? OR l.response LIKE ? OR u.name LIKE ?`,
			p, p, p,
		).Scan(&count)
	} else {
		err = db.QueryRow(`SELECT COUNT(*) FROM logs l JOIN users u ON u.id=l.user_id`).Scan(&count)
	}
	return count, err
}

func getLogsPage(search string, limit, offset int) ([]LogRow, error) {
	var (
		sqlRows *sql.Rows
		err     error
	)
	if search != "" {
		p := "%" + search + "%"
		sqlRows, err = db.Query(
			`SELECT l.id, u.name, COALESCE(l.model,''), l.prompt, l.response, l.ts
			FROM logs l JOIN users u ON u.id=l.user_id
			WHERE l.prompt LIKE ? OR l.response LIKE ? OR u.name LIKE ?
			ORDER BY l.ts DESC LIMIT ? OFFSET ?`,
			p, p, p, limit, offset,
		)
	} else {
		sqlRows, err = db.Query(
			`SELECT l.id, u.name, COALESCE(l.model,''), l.prompt, l.response, l.ts
			FROM logs l JOIN users u ON u.id=l.user_id
			ORDER BY l.ts DESC LIMIT ? OFFSET ?`,
			limit, offset,
		)
	}
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()
	var out []LogRow
	for sqlRows.Next() {
		var r LogRow
		sqlRows.Scan(&r.ID, &r.Name, &r.Model, &r.Prompt, &r.Response, &r.TS)
		out = append(out, r)
	}
	return out, sqlRows.Err()
}

// ── Admin queries ─────────────────────────────────────────────────────────────

type AdminUser struct {
	ID           int
	Email        string
	PasswordHash string
}

func getAdmin() (*AdminUser, error) {
	var a AdminUser
	err := db.QueryRow("SELECT id, email, password_hash FROM admin_user LIMIT 1").
		Scan(&a.ID, &a.Email, &a.PasswordHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

func updateAdminPassword(adminID int, hash string) error {
	_, err := db.Exec("UPDATE admin_user SET password_hash=? WHERE id=?", hash, adminID)
	return err
}

// ── Settings ──────────────────────────────────────────────────────────────────

func getSetting(key, def string) string {
	var val string
	err := db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&val)
	if err != nil {
		return def
	}
	return val
}

func setSetting(key, value string) error {
	_, err := db.Exec(
		"INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		key, value,
	)
	return err
}

// ── System reset ──────────────────────────────────────────────────────────────

func resetSystem() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tx.Exec("DELETE FROM logs")
	tx.Exec("DELETE FROM rate_limit_entries")
	return tx.Commit()
}

// ── Rate limiter ──────────────────────────────────────────────────────────────

const rateLimitWindow = 60.0 // seconds

func isAllowed(userID, limit int) bool {
	cutoff := float64(time.Now().Unix()) - rateLimitWindow
	db.Exec("DELETE FROM rate_limit_entries WHERE timestamp < ?", cutoff)
	var count int
	db.QueryRow("SELECT COUNT(*) FROM rate_limit_entries WHERE user_id=?", userID).Scan(&count)
	if count >= limit {
		return false
	}
	db.Exec("INSERT INTO rate_limit_entries(user_id, timestamp) VALUES(?,?)",
		userID, float64(time.Now().Unix()))
	return true
}

func cleanupRateLimitEntries() {
	cutoff := float64(time.Now().Unix()) - rateLimitWindow
	db.Exec("DELETE FROM rate_limit_entries WHERE timestamp < ?", cutoff)
}

// ─────────────────────────────── Auth utilities ────────────────────────────────

// hashPassword creates a Werkzeug-compatible pbkdf2:sha256 hash.
func hashPassword(password string) string {
	saltBytes := make([]byte, 16)
	rand.Read(saltBytes)
	salt := hex.EncodeToString(saltBytes) // 32-char hex string
	iterations := 260000
	dk := gocrypto.Key([]byte(password), []byte(salt), iterations, 32, sha256.New)
	hash := hex.EncodeToString(dk)
	return fmt.Sprintf("pbkdf2:sha256:%d$%s$%s", iterations, salt, hash)
}

// verifyPassword checks a Werkzeug pbkdf2:sha256 hash.
func verifyPassword(hashStr, password string) bool {
	parts := strings.SplitN(hashStr, "$", 3)
	if len(parts) != 3 {
		return false
	}
	methodParts := strings.Split(parts[0], ":")
	if len(methodParts) < 3 || methodParts[0] != "pbkdf2" || methodParts[1] != "sha256" {
		return false
	}
	iterations, err := strconv.Atoi(methodParts[2])
	if err != nil || iterations <= 0 {
		return false
	}
	salt := parts[1]
	expectedHash := parts[2]
	dk := gocrypto.Key([]byte(password), []byte(salt), iterations, 32, sha256.New)
	computed := hex.EncodeToString(dk)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(expectedHash)) == 1
}

// generateAPIKey returns a URL-safe random API key (~27 chars).
func generateAPIKey() string {
	b := make([]byte, 20)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func validateName(name string) bool {
	n := strings.TrimSpace(name)
	return len(n) >= 1 && len(n) <= 100
}

var (
	reUppercase = regexp.MustCompile(`[A-Z]`)
	reLowercase = regexp.MustCompile(`[a-z]`)
	reDigit     = regexp.MustCompile(`\d`)
)

func validatePassword(password string) (bool, string) {
	if len(password) < 8 {
		return false, "Password must be at least 8 characters"
	}
	if !reUppercase.MatchString(password) {
		return false, "Password must contain at least one uppercase letter"
	}
	if !reLowercase.MatchString(password) {
		return false, "Password must contain at least one lowercase letter"
	}
	if !reDigit.MatchString(password) {
		return false, "Password must contain at least one digit"
	}
	return true, ""
}

func truncateInput(text string) string {
	if len(text) <= cfg.MaxMsgLen {
		return text
	}
	return text[:cfg.MaxMsgLen]
}

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

func addFlash(w http.ResponseWriter, r *http.Request, category, message string) {
	sess := getSession(r)
	var flashes []FlashMsg
	if raw, ok := sess.Values["flashes"].(string); ok && raw != "" {
		json.Unmarshal([]byte(raw), &flashes)
	}
	flashes = append(flashes, FlashMsg{category, message})
	b, _ := json.Marshal(flashes)
	sess.Values["flashes"] = string(b)
	sess.Save(r, w)
}

func consumeFlashes(w http.ResponseWriter, r *http.Request, sess *sessions.Session) []FlashMsg {
	raw, ok := sess.Values["flashes"].(string)
	if !ok || raw == "" {
		return nil
	}
	var flashes []FlashMsg
	json.Unmarshal([]byte(raw), &flashes)
	sess.Values["flashes"] = ""
	sess.Save(r, w)
	return flashes
}

// ─────────────────────────────── Ollama URL helper ─────────────────────────────

func getOllamaBaseURL() string {
	savedURL := getSetting("ollama_url", "")
	if savedURL != "" {
		savedPort := getSetting("ollama_port", "11434")
		return savedURL + ":" + savedPort
	}
	parsed, err := url.Parse(cfg.OllamaURL)
	if err != nil {
		return "http://127.0.0.1:11434"
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "http"
	}
	host := parsed.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	port := parsed.Port()
	if port == "" {
		port = "11434"
	}
	return fmt.Sprintf("%s://%s:%s", scheme, host, port)
}

func parseOllamaSettings() (string, int) {
	savedURL := getSetting("ollama_url", "")
	if savedURL != "" {
		port, _ := strconv.Atoi(getSetting("ollama_port", "11434"))
		if port == 0 {
			port = 11434
		}
		return savedURL, port
	}
	parsed, _ := url.Parse(cfg.OllamaURL)
	host := "http://" + parsed.Hostname()
	port := 11434
	if p := parsed.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	return host, port
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

// ─────────────────────────── Ollama probe cache ───────────────────────────────

type ollamaProbeData struct {
	status  bool
	models  []string
	details []ModelDetail
	url     string
	port    int
}

var (
	ollamaProbeMu    sync.Mutex
	ollamaProbeState ollamaProbeData
	ollamaProbeExpiry time.Time
)

const ollamaProbeTTL = 15 * time.Second

// probeOllamaCached returns Ollama status, model names, and model details,
// caching the result for ollamaProbeTTL to avoid blocking every page load.
func probeOllamaCached(baseURL string, port int) (bool, []string, []ModelDetail) {
	ollamaProbeMu.Lock()
	defer ollamaProbeMu.Unlock()
	if time.Now().Before(ollamaProbeExpiry) &&
		ollamaProbeState.url == baseURL &&
		ollamaProbeState.port == port {
		return ollamaProbeState.status, ollamaProbeState.models, ollamaProbeState.details
	}
	status, models, details := probeOllamaFull(baseURL, port)
	ollamaProbeState = ollamaProbeData{
		status: status, models: models, details: details,
		url: baseURL, port: port,
	}
	ollamaProbeExpiry = time.Now().Add(ollamaProbeTTL)
	return status, models, details
}

// probeOllamaFull checks Ollama connectivity and returns status, model names,
// and model details in a single pair of HTTP calls.
func probeOllamaFull(baseURL string, port int) (bool, []string, []ModelDetail) {
	endpoint := fmt.Sprintf("%s:%d", baseURL, port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(endpoint + "/api/version")
	if err != nil || resp.StatusCode != 200 {
		return false, nil, nil
	}
	resp.Body.Close()

	client2 := &http.Client{Timeout: 5 * time.Second}
	resp2, err := client2.Get(endpoint + "/api/tags")
	if err != nil || resp2.StatusCode != 200 {
		return true, nil, nil
	}
	defer resp2.Body.Close()
	var tags map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&tags)
	models, _ := tags["models"].([]interface{})
	var names []string
	var details []ModelDetail
	for _, m := range models {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := mm["name"].(string)
		names = append(names, name)
		sizeB := toInt(mm["size"])
		var sizeStr string
		if sizeB >= 1_000_000_000 {
			sizeStr = fmt.Sprintf("%.1f GB", float64(sizeB)/1e9)
		} else {
			sizeStr = fmt.Sprintf("%d MB", sizeB/1_000_000)
		}
		details = append(details, ModelDetail{Name: name, Size: sizeStr})
	}
	return true, names, details
}

// ─────────────────────────────── Background tasks ─────────────────────────────

func startBackgroundTasks() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cleanupRateLimitEntries()
		}
	}()
}

// ─────────────────────────────── CORS ─────────────────────────────────────────

var corsHeaders = map[string]string{
	"Access-Control-Allow-Origin":  "*",
	"Access-Control-Allow-Methods": "GET, POST, OPTIONS",
	"Access-Control-Allow-Headers": "Authorization, Content-Type",
	"Access-Control-Max-Age":       "86400",
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

	// Middleware applied to all routes
	r.Use(recoveryMiddleware)
	r.Use(corsMiddleware)

	// Public
	r.HandleFunc("/", handleIndex).Methods("GET")
	r.HandleFunc("/health", handleHealth).Methods("GET")
	r.HandleFunc("/admin", handleAdminLogin).Methods("POST")
	r.HandleFunc("/logout", handleLogout).Methods("GET")

	// Admin UI
	r.HandleFunc("/admin/panel", handleAdminPanel).Methods("GET")
	r.HandleFunc("/admin/settings-page", handleAdminSettingsPage).Methods("GET")
	r.HandleFunc("/admin/ollama-test", handleOllamaTest).Methods("GET")
	r.HandleFunc("/admin/ollama-pull-stream", handleOllamaPullStream).Methods("GET")
	r.HandleFunc("/admin/ollama-pull-search-stream", handleOllamaPullSearchStream).Methods("GET")
	r.HandleFunc("/admin/ollama-delete-model", handleOllamaDeleteModel).Methods("POST")
	r.HandleFunc("/admin/ollama-settings", handleOllamaSettings).Methods("POST")
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

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("panic recovered", "err", err, "path", r.URL.Path)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
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
	}

	// DB
	if err := initDB(); err != nil {
		slog.Error("Database initialization failed", "err", err)
		os.Exit(1)
	}

	// Templates
	initTemplates()

	// Background tasks
	startBackgroundTasks()

	// Router
	router := setupRouter()

	addr := "0.0.0.0:" + cfg.Port
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		slog.Info("Gateway42 listening (HTTPS)", "addr", addr, "cert", cfg.TLSCert)
		if err := http.ListenAndServeTLS(addr, cfg.TLSCert, cfg.TLSKey, router); err != nil {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	} else {
		slog.Info("Gateway42 listening (HTTP)", "addr", addr)
		if err := http.ListenAndServe(addr, router); err != nil {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}
}
