package main

import (
	"container/list"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// tableHasColumn reports whether a table already has the named column, which is
// how the migrations decide whether an ALTER is needed. The scan error is
// returned rather than dropped: swallowing it reads as "column absent" and
// triggers an ALTER that then fails with a message describing the symptom
// rather than the cause.
//
// table is interpolated because PRAGMA does not accept bound parameters for it.
// Every call site passes a string literal; do not hand it anything derived from
// a request.
func tableHasColumn(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("PRAGMA table_info(%s): %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// txCount runs a single-value COUNT query inside a migration transaction.
// It exists so the error cannot be dropped: a caller that ignores it gets 0,
// which is a legitimate count and therefore indistinguishable from failure.
func txCount(tx *sql.Tx, query string) (int, error) {
	var n int
	if err := tx.QueryRow(query).Scan(&n); err != nil {
		return 0, fmt.Errorf("%s: %w", query, err)
	}
	return n, nil
}

// sha256Hex returns the hex-encoded SHA-256 hash of s.
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// fts5Enabled is set during initDB. When false, log search falls back to LIKE
// (slow on large tables, but correct).
var fts5Enabled bool

// ftsPhrase wraps a user-entered search term as an FTS5 phrase, escaping any
// embedded double-quotes by doubling them. With the trigram tokenizer this
// gives substring-equivalent matching.
func ftsPhrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// looksHashed reports whether s looks like a 64-char lower-hex SHA-256.
func looksHashed(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		if !isDigit && !isLowerHex {
			return false
		}
	}
	return true
}

// ── Init ──────────────────────────────────────────────────────────────────────

func initDB() error {
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0755); err != nil {
		return err
	}
	// Remove zero-byte WAL/SHM files left by a previous crash before any data
	// was written; a 0-byte WAL causes SQLITE_IOERR_SHORT_READ (522).
	if fi, err := os.Stat(cfg.DBPath + "-wal"); err == nil && fi.Size() == 0 {
		// Failing here is worth aborting for: leaving the truncated WAL in
		// place means SQLite opens it and fails with SQLITE_IOERR_SHORT_READ,
		// whose message mentions neither the file nor the reason it is bad.
		if err := os.Remove(cfg.DBPath + "-wal"); err != nil {
			return fmt.Errorf("removing zero-byte WAL: %w", err)
		}
		// The -shm only exists while something has the database mapped, so its
		// absence here is the normal case rather than a problem.
		if err := os.Remove(cfg.DBPath + "-shm"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing stale SHM: %w", err)
		}
	}

	// Writer pool: single connection. SQLite supports one writer at a time;
	// pinning to one connection avoids contention and keeps prepared-statement
	// state consistent for migrations.
	dsnWrite := cfg.DBPath + "?_pragma=journal_mode%3DWAL&_pragma=busy_timeout%3D5000&_pragma=synchronous%3DNORMAL"
	var err error
	db, err = sql.Open("sqlite", dsnWrite)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := createSchema(); err != nil {
		return err
	}

	// Reader pool: opened after the writer has created the DB + WAL files so
	// mode=ro can attach safely. With WAL, many readers run concurrently with
	// the single writer. Read-only enforcement makes accidental writes via
	// this pool fail loudly instead of silently contending.
	dsnRead := "file:" + cfg.DBPath + "?mode=ro&_pragma=busy_timeout%3D5000"
	dbRead, err = sql.Open("sqlite", dsnRead)
	if err != nil {
		return err
	}
	dbRead.SetMaxOpenConns(8)
	dbRead.SetMaxIdleConns(4)
	dbRead.SetConnMaxLifetime(0)

	// Verify reader actually opens — sql.Open is lazy.
	if err := dbRead.Ping(); err != nil {
		return fmt.Errorf("reader pool ping: %w", err)
	}
	return nil
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
		`CREATE TABLE IF NOT EXISTS request_logs(
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			ts              TEXT NOT NULL,
			method          TEXT NOT NULL,
			path            TEXT NOT NULL,
			client_ip       TEXT NOT NULL,
			user_name       TEXT NOT NULL DEFAULT '',
			status_code     INTEGER NOT NULL,
			request_id      TEXT NOT NULL DEFAULT '',
			model_requested TEXT NOT NULL DEFAULT '',
			model_served    TEXT NOT NULL DEFAULT '',
			engine_name     TEXT NOT NULL DEFAULT '',
			engine_type     TEXT NOT NULL DEFAULT '',
			streamed        INTEGER NOT NULL DEFAULT 0,
			ttft_ms         INTEGER NOT NULL DEFAULT 0,
			duration_ms     INTEGER NOT NULL DEFAULT 0,
			in_tokens       INTEGER NOT NULL DEFAULT 0,
			out_tokens      INTEGER NOT NULL DEFAULT 0,
			stop_reason     TEXT NOT NULL DEFAULT '',
			outcome         TEXT NOT NULL DEFAULT '',
			upstream_error  TEXT NOT NULL DEFAULT '',
			user_agent      TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_user_id ON logs(user_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_api_key ON users(api_key)`,
		`CREATE INDEX IF NOT EXISTS idx_rate_limit_user_timestamp ON rate_limit_entries(user_id, timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_ts ON request_logs(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs(ts)`,
		// getUserLogs filters by user and orders by ts. idx_logs_user_id
		// satisfies the filter but leaves SQLite to sort every matching row;
		// this composite serves the whole query in index order, which is the
		// difference between a seek and a sort once a user has thousands of
		// rows. The single-column index stays: the retention prune and any
		// user-only filter can still use it, and SQLite picks per query.
		`CREATE INDEX IF NOT EXISTS idx_logs_user_ts ON logs(user_id, ts)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
	}

	// Migration: rename users.email → users.name
	hasName, err := tableHasColumn(tx, "users", "name")
	if err != nil {
		return err
	}
	if !hasName {
		if _, err := tx.Exec("ALTER TABLE users RENAME COLUMN email TO name"); err != nil {
			return err
		}
	}

	// Migration: add model column to logs
	hasModel, err := tableHasColumn(tx, "logs", "model")
	if err != nil {
		return err
	}
	if !hasModel {
		if _, err := tx.Exec("ALTER TABLE logs ADD COLUMN model TEXT"); err != nil {
			return err
		}
	}

	// Migration: add detail columns to request_logs. One ALTER per column:
	// SQLite has no ADD COLUMN IF NOT EXISTS, and tableHasColumn answers that
	// per column, so a half-applied older migration completes rather than
	// failing startup. Defaults keep every pre-detail row readable.
	for _, col := range []struct{ name, decl string }{
		{"request_id", "TEXT NOT NULL DEFAULT ''"},
		{"model_requested", "TEXT NOT NULL DEFAULT ''"},
		{"model_served", "TEXT NOT NULL DEFAULT ''"},
		{"engine_name", "TEXT NOT NULL DEFAULT ''"},
		{"engine_type", "TEXT NOT NULL DEFAULT ''"},
		{"streamed", "INTEGER NOT NULL DEFAULT 0"},
		{"ttft_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"duration_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"in_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"out_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"stop_reason", "TEXT NOT NULL DEFAULT ''"},
		{"outcome", "TEXT NOT NULL DEFAULT ''"},
		{"upstream_error", "TEXT NOT NULL DEFAULT ''"},
		{"user_agent", "TEXT NOT NULL DEFAULT ''"},
	} {
		has, err := tableHasColumn(tx, "request_logs", col.name)
		if err != nil {
			return err
		}
		if !has {
			if _, err := tx.Exec("ALTER TABLE request_logs ADD COLUMN " + col.name + " " + col.decl); err != nil {
				return err
			}
		}
	}

	// Migration: hash any plaintext API keys.
	// Older builds stored raw keys; we now store sha256(rawKey). Detect
	// non-64-hex rows and rehash in place so existing clients keep working.
	krows, err := tx.Query("SELECT id, api_key FROM users")
	if err != nil {
		return err
	}
	type kv struct {
		id  int
		raw string
	}
	var toHash []kv
	for krows.Next() {
		var id int
		var key string
		if err := krows.Scan(&id, &key); err == nil {
			if !looksHashed(key) {
				toHash = append(toHash, kv{id, key})
			}
		}
	}
	// Closed explicitly rather than deferred: the UPDATEs below run on the same
	// single-writer connection, and SQLite will not let them proceed while this
	// cursor still holds it. A failure here therefore predicts a failure there,
	// so report it at the point that explains it.
	if err := krows.Close(); err != nil {
		return fmt.Errorf("api key migration: releasing scan cursor: %w", err)
	}
	for _, k := range toHash {
		if _, err := tx.Exec("UPDATE users SET api_key=? WHERE id=?", sha256Hex(k.raw), k.id); err != nil {
			return fmt.Errorf("api key migration: %w", err)
		}
	}
	if len(toHash) > 0 {
		slog.Info("Migrated plaintext API keys to SHA-256", "count", len(toHash))
	}

	// FTS5 virtual tables for log search. Trigram tokenizer gives
	// substring-match semantics (LIKE-equivalent) without the table scan.
	// If FTS creation fails (older sqlite build without FTS5), we keep
	// running and fall back to LIKE in the query helpers.

	// Migration: request_logs_fts gained model_served and engine_name. A
	// contentless FTS table cannot be re-columned in place, and IF NOT
	// EXISTS would keep the old shape silently, so a stale table (with its
	// triggers, which belong to the base table and would keep writing the
	// old column list) is dropped here. The backfill below repopulates it
	// from request_logs.
	hasEngineCol, err := tableHasColumn(tx, "request_logs_fts", "engine_name")
	if err != nil {
		return err
	}
	if !hasEngineCol {
		for _, s := range []string{
			`DROP TRIGGER IF EXISTS rlogs_ai`,
			`DROP TRIGGER IF EXISTS rlogs_ad`,
			`DROP TABLE IF EXISTS request_logs_fts`,
		} {
			if _, err := tx.Exec(s); err != nil {
				return err
			}
		}
	}

	ftsStmts := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS logs_fts USING fts5(
			prompt, response, user_name, content='', tokenize='trigram')`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS request_logs_fts USING fts5(
			path, client_ip, user_name, model_served, engine_name, content='', tokenize='trigram')`,
		`CREATE TRIGGER IF NOT EXISTS logs_ai AFTER INSERT ON logs BEGIN
			INSERT INTO logs_fts(rowid, prompt, response, user_name)
			VALUES (new.id, COALESCE(new.prompt,''), COALESCE(new.response,''),
				COALESCE((SELECT name FROM users WHERE id=new.user_id), ''));
		END`,
		`CREATE TRIGGER IF NOT EXISTS logs_ad AFTER DELETE ON logs BEGIN
			INSERT INTO logs_fts(logs_fts, rowid, prompt, response, user_name)
			VALUES ('delete', old.id, COALESCE(old.prompt,''),
				COALESCE(old.response,''),
				COALESCE((SELECT name FROM users WHERE id=old.user_id), ''));
		END`,
		`CREATE TRIGGER IF NOT EXISTS rlogs_ai AFTER INSERT ON request_logs BEGIN
			INSERT INTO request_logs_fts(rowid, path, client_ip, user_name, model_served, engine_name)
			VALUES (new.id, new.path, new.client_ip, new.user_name, new.model_served, new.engine_name);
		END`,
		`CREATE TRIGGER IF NOT EXISTS rlogs_ad AFTER DELETE ON request_logs BEGIN
			INSERT INTO request_logs_fts(request_logs_fts, rowid, path, client_ip, user_name, model_served, engine_name)
			VALUES ('delete', old.id, old.path, old.client_ip, old.user_name, old.model_served, old.engine_name);
		END`,
	}
	ftsOK := true
	for _, s := range ftsStmts {
		if _, err := tx.Exec(s); err != nil {
			slog.Warn("FTS5 setup failed, falling back to LIKE search", "err", err)
			ftsOK = false
			break
		}
	}
	if ftsOK {
		// One-shot backfill: populate FTS tables from existing rows if empty.
		//
		// A failed count is indistinguishable from a count of zero, and zero is
		// precisely what triggers a backfill, so an unchecked scan here would
		// re-run the INSERT on every startup against a table that may already be
		// populated. FTS is optional, so a count that cannot be read degrades to
		// the LIKE fallback rather than failing the whole migration.
		ftsLogsCount, errFTSLogs := txCount(tx, "SELECT COUNT(*) FROM logs_fts")
		baseLogsCount, errBaseLogs := txCount(tx, "SELECT COUNT(*) FROM logs")
		switch {
		case errFTSLogs != nil || errBaseLogs != nil:
			slog.Warn("logs_fts count failed, falling back to LIKE search",
				"err", errors.Join(errFTSLogs, errBaseLogs))
			ftsOK = false
		case ftsLogsCount == 0 && baseLogsCount > 0:
			if _, err := tx.Exec(`
				INSERT INTO logs_fts(rowid, prompt, response, user_name)
				SELECT l.id, COALESCE(l.prompt,''), COALESCE(l.response,''),
					COALESCE(u.name, '')
				FROM logs l LEFT JOIN users u ON u.id=l.user_id`); err != nil {
				slog.Warn("logs_fts backfill failed", "err", err)
				ftsOK = false
			} else {
				slog.Info("logs_fts backfilled", "rows", baseLogsCount)
			}
		}
		ftsRLCount, errFTSRL := txCount(tx, "SELECT COUNT(*) FROM request_logs_fts")
		baseRLCount, errBaseRL := txCount(tx, "SELECT COUNT(*) FROM request_logs")
		switch {
		case errFTSRL != nil || errBaseRL != nil:
			slog.Warn("request_logs_fts count failed, falling back to LIKE search",
				"err", errors.Join(errFTSRL, errBaseRL))
			ftsOK = false
		case ftsRLCount == 0 && baseRLCount > 0:
			if _, err := tx.Exec(`
				INSERT INTO request_logs_fts(rowid, path, client_ip, user_name, model_served, engine_name)
				SELECT id, path, client_ip, user_name, model_served, engine_name FROM request_logs`); err != nil {
				slog.Warn("request_logs_fts backfill failed", "err", err)
				ftsOK = false
			} else {
				slog.Info("request_logs_fts backfilled", "rows", baseRLCount)
			}
		}
	}
	fts5Enabled = ftsOK

	// Seed admin user if none exists. Unlike the FTS counts this one aborts the
	// migration: a failed count reads as zero, and zero means "seed an admin",
	// so swallowing the error risks an INSERT against a table that already has
	// one — silently, on a database whose state we could not read.
	exists, err := txCount(tx, "SELECT COUNT(*) FROM admin_user")
	if err != nil {
		return err
	}
	if exists == 0 {
		pw := cfg.AdminPassword
		if pw == "" {
			pw = "admin123"
		}
		if _, err := tx.Exec(
			"INSERT INTO admin_user(email, password_hash, created_at) VALUES(?,?,?)",
			"admin", hashPassword(pw), time.Now().UTC().Format(time.RFC3339),
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

// ── User cache (size-capped LRU with TTL) ─────────────────────────────────────

const (
	userCacheTTL = 5 * time.Minute
	userCacheCap = 4096
)

type userCacheEntry struct {
	key  interface{}
	user *User
	exp  time.Time
}

var (
	userCacheMu  sync.Mutex
	userCacheLRU = list.New() // front = most recently used
	userCacheIdx = map[interface{}]*list.Element{}
)

func userCacheGet(key interface{}) *User {
	userCacheMu.Lock()
	defer userCacheMu.Unlock()
	el, ok := userCacheIdx[key]
	if !ok {
		return nil
	}
	e := el.Value.(*userCacheEntry)
	if time.Now().After(e.exp) {
		userCacheLRU.Remove(el)
		delete(userCacheIdx, key)
		return nil
	}
	userCacheLRU.MoveToFront(el)
	return e.user
}

func userCacheSet(key interface{}, u *User) {
	userCacheMu.Lock()
	defer userCacheMu.Unlock()
	if el, ok := userCacheIdx[key]; ok {
		e := el.Value.(*userCacheEntry)
		e.user = u
		e.exp = time.Now().Add(userCacheTTL)
		userCacheLRU.MoveToFront(el)
		return
	}
	e := &userCacheEntry{key: key, user: u, exp: time.Now().Add(userCacheTTL)}
	el := userCacheLRU.PushFront(e)
	userCacheIdx[key] = el
	for userCacheLRU.Len() > userCacheCap {
		tail := userCacheLRU.Back()
		if tail == nil {
			break
		}
		userCacheLRU.Remove(tail)
		delete(userCacheIdx, tail.Value.(*userCacheEntry).key)
	}
}

func invalidateUserCache(userID int) {
	userCacheMu.Lock()
	defer userCacheMu.Unlock()
	idKey := fmt.Sprintf("id:%d", userID)
	if el, ok := userCacheIdx[idKey]; ok {
		userCacheLRU.Remove(el)
		delete(userCacheIdx, idKey)
	}
	for k, el := range userCacheIdx {
		s, isStr := k.(string)
		if !isStr || !strings.HasPrefix(s, "api:") {
			continue
		}
		if el.Value.(*userCacheEntry).user.ID == userID {
			userCacheLRU.Remove(el)
			delete(userCacheIdx, k)
		}
	}
}

// ── User queries ──────────────────────────────────────────────────────────────

// getUserByAPIKey looks up a user by raw API key, hashing it before querying.
func getUserByAPIKey(rawKey string) (*User, error) {
	hash := sha256Hex(rawKey)
	cacheKey := "api:" + hash
	if u := userCacheGet(cacheKey); u != nil {
		return u, nil
	}
	row := dbRead.QueryRow(
		"SELECT id, name, api_key, status, rate_limit, created_at FROM users WHERE api_key=?", hash,
	)
	u, err := scanUser(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	userCacheSet(cacheKey, u)
	return u, nil
}

func getUserByID(id int) (*User, error) {
	key := fmt.Sprintf("id:%d", id)
	if u := userCacheGet(key); u != nil {
		return u, nil
	}
	row := dbRead.QueryRow(
		"SELECT id, name, api_key, status, rate_limit, created_at FROM users WHERE id=?", id,
	)
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

func scanUser(row *sql.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.APIKey, &u.Status, &u.RateLimit, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func getAllUsers() ([]User, error) {
	rows, err := dbRead.Query(
		"SELECT id, name, api_key, status, rate_limit, created_at FROM users ORDER BY created_at DESC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.APIKey, &u.Status, &u.RateLimit, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// createUser stores a new user, hashing the raw API key before writing.
func createUser(name, rawKey string, rateLimit int) error {
	_, err := db.Exec(
		"INSERT INTO users(name, api_key, status, rate_limit, created_at) VALUES(?,?,?,?,?)",
		name, sha256Hex(rawKey), "disabled", rateLimit, time.Now().UTC().Format(time.RFC3339),
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

// resetUserAPIKey updates a user's API key, hashing the raw key before writing.
func resetUserAPIKey(userID int, rawKey string) error {
	_, err := db.Exec("UPDATE users SET api_key=? WHERE id=?", sha256Hex(rawKey), userID)
	if err == nil {
		invalidateUserCache(userID)
	}
	return err
}

func updateUserRateLimit(userID, rateLimit int) error {
	_, err := db.Exec("UPDATE users SET rate_limit=? WHERE id=?", rateLimit, userID)
	if err == nil {
		invalidateUserCache(userID)
		dropUserLimiter(userID)
	}
	return err
}

func deleteUser(userID int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM logs WHERE user_id=?", userID); err != nil {
		return fmt.Errorf("delete user logs: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM users WHERE id=?", userID); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	invalidateUserCache(userID)
	dropUserLimiter(userID)
	return nil
}

// ── Log queries ───────────────────────────────────────────────────────────────

// logInteraction enqueues an audit row. The actual INSERT — and the JSON
// encoding of anything that is not already a string — is performed by the
// batched writer in logwriter.go, off the request path.
func logInteraction(userID int, prompt, response interface{}, model string) {
	enqueueInteractionLog(userID, prompt, response, model)
}

func getUserLogs(userID int) ([]UserLogRow, error) {
	rows, err := dbRead.Query(
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
		if err := rows.Scan(&r.ID, &model, &r.Prompt, &r.Response, &r.TS); err != nil {
			return nil, err
		}
		r.Model = model.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Request log queries ───────────────────────────────────────────────────────

// insertRequestLog enqueues a request-log row. The actual INSERT is
// performed by the batched writer in logwriter.go. detail carries the
// per-request facts handlers collected; it may be nil for callers that
// predate the detail collector.
func insertRequestLog(method, path, clientIP, userName string, status int, detail *requestDetail) {
	enqueueRequestLog(method, path, clientIP, userName, status, detail)
}

func getRequestLogsCount(search string) (int, error) {
	var count int
	var err error
	switch {
	case search != "" && fts5Enabled:
		err = dbRead.QueryRow(
			`SELECT COUNT(*) FROM request_logs_fts WHERE request_logs_fts MATCH ?`,
			ftsPhrase(search),
		).Scan(&count)
	case search != "":
		p := "%" + search + "%"
		err = dbRead.QueryRow(
			`SELECT COUNT(*) FROM request_logs WHERE user_name LIKE ? OR path LIKE ? OR client_ip LIKE ?
			OR model_served LIKE ? OR engine_name LIKE ?`,
			p, p, p, p, p,
		).Scan(&count)
	default:
		err = dbRead.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&count)
	}
	return count, err
}

// requestLogCols is the column list shared by every request_logs read. It is
// one list rather than three repeated literals because the scan order has to
// match the SELECT order exactly, and 21 columns repeated per query is three
// chances to drift.
var requestLogCols = []string{
	"id", "ts", "method", "path", "client_ip", "user_name", "status_code",
	"request_id", "model_requested", "model_served", "engine_name", "engine_type",
	"streamed", "ttft_ms", "duration_ms", "in_tokens", "out_tokens",
	"stop_reason", "outcome", "upstream_error", "user_agent",
}

var (
	requestLogColumns  = strings.Join(requestLogCols, ", ")
	requestLogColumnsR = "r." + strings.Join(requestLogCols, ", r.")
)

func getRequestLogsPage(search string, limit, offset int) ([]RequestLogRow, error) {
	var (
		sqlRows *sql.Rows
		err     error
	)
	switch {
	case search != "" && fts5Enabled:
		sqlRows, err = dbRead.Query(
			`SELECT `+requestLogColumnsR+`
			FROM request_logs_fts f
			JOIN request_logs r ON r.id = f.rowid
			WHERE request_logs_fts MATCH ?
			ORDER BY r.ts DESC LIMIT ? OFFSET ?`,
			ftsPhrase(search), limit, offset,
		)
	case search != "":
		p := "%" + search + "%"
		sqlRows, err = dbRead.Query(
			`SELECT `+requestLogColumns+` FROM request_logs
			WHERE user_name LIKE ? OR path LIKE ? OR client_ip LIKE ?
			OR model_served LIKE ? OR engine_name LIKE ?
			ORDER BY ts DESC LIMIT ? OFFSET ?`,
			p, p, p, p, p, limit, offset,
		)
	default:
		sqlRows, err = dbRead.Query(
			`SELECT `+requestLogColumns+` FROM request_logs
			ORDER BY ts DESC LIMIT ? OFFSET ?`,
			limit, offset,
		)
	}
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()
	var out []RequestLogRow
	for sqlRows.Next() {
		var r RequestLogRow
		if err := sqlRows.Scan(&r.ID, &r.TS, &r.Method, &r.Path, &r.ClientIP, &r.UserName, &r.StatusCode,
			&r.RequestID, &r.ModelRequested, &r.ModelServed, &r.EngineName, &r.EngineType,
			&r.Streamed, &r.TTFTMs, &r.DurationMs, &r.InTokens, &r.OutTokens,
			&r.StopReason, &r.Outcome, &r.UpstreamError, &r.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, sqlRows.Err()
}

// ── Usage queries ─────────────────────────────────────────────────────────────

// usageRow is one (client, model) aggregate over a window. It stays flat and
// unexported because the dashboard wants clients with their models nested,
// which reads better as a Go grouping pass than as a second result set.
type usageRow struct {
	UserName   string
	Model      string
	Requests   int
	InTokens   int
	OutTokens  int
	Streamed   int
	Unreported int
	UserID     sql.NullInt64
	Status     sql.NullString
}

// getUsageByClient aggregates token usage per client and model for rows at or
// after an RFC3339 UTC cut-off.
//
// since is a string rather than a time.Time on purpose: ts is written as
// fixed-width RFC3339 in UTC, so a text comparison is chronological and
// idx_request_logs_ts stays usable — the same property pruneLogs depends on.
//
// The path filter is load-bearing. requestLoggingMiddleware logs every /v1/
// path, and /v1/models and /v1/messages/count_tokens always carry zero tokens,
// so without it a client that only polls the model list appears to be a heavy
// free user and the request counts stop meaning "completions".
//
// A row whose counts are both zero is tallied into Unreported instead of being
// counted as free, because "the engine never answered" and "the stream died
// before its usage line" are indistinguishable from a genuine zero-token
// completion in the stored data.
//
// The join to users is a LEFT JOIN: user_name is denormalised into request_logs
// at insert, so a client deleted last week still owns rows, and dropping them
// would make the window's history depend on who is still registered.
func getUsageByClient(since string) ([]usageRow, error) {
	rows, err := dbRead.Query(
		`SELECT a.user_name, a.model, a.requests, a.in_tokens, a.out_tokens,
			a.streamed, a.unreported, u.id, u.status
		FROM (
			SELECT user_name,
				COALESCE(NULLIF(model_served,''), NULLIF(model_requested,''), '(unknown)') AS model,
				COUNT(*) AS requests,
				SUM(in_tokens) AS in_tokens,
				SUM(out_tokens) AS out_tokens,
				SUM(streamed) AS streamed,
				SUM(CASE WHEN in_tokens=0 AND out_tokens=0 THEN 1 ELSE 0 END) AS unreported
			FROM request_logs
			WHERE ts >= ? AND path IN ('/v1/chat/completions','/v1/messages')
			GROUP BY user_name, model
		) a
		LEFT JOIN users u ON u.name = a.user_name
		ORDER BY a.in_tokens + a.out_tokens DESC, a.user_name, a.model`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []usageRow
	for rows.Next() {
		var r usageRow
		if err := rows.Scan(&r.UserName, &r.Model, &r.Requests, &r.InTokens, &r.OutTokens,
			&r.Streamed, &r.Unreported, &r.UserID, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Admin queries ─────────────────────────────────────────────────────────────

type AdminUser struct {
	ID           int
	Email        string
	PasswordHash string
}

func getAdmin() (*AdminUser, error) {
	var a AdminUser
	err := dbRead.QueryRow("SELECT id, email, password_hash FROM admin_user LIMIT 1").
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
	if _, err := tx.Exec("DELETE FROM logs"); err != nil {
		return fmt.Errorf("clear interaction logs: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM request_logs"); err != nil {
		return fmt.Errorf("clear request logs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// Rate-limit state lives in memory (ratelimit.go), not in a table, so
	// clearing it is a separate step from the DELETEs above.
	resetAllLimiters()

	// VACUUM reclaims the file space freed by the DELETEs. It cannot run
	// inside a transaction. Best-effort — failure here is not fatal.
	if _, err := db.Exec("VACUUM"); err != nil {
		slog.Warn("VACUUM after reset failed", "err", err)
	}
	return nil
}

// ── Log retention ─────────────────────────────────────────────────────────────

// pruneLogs deletes log rows older than cfg.LogRetentionDays and reports how
// many went from each table. A retention of 0 disables pruning entirely.
//
// Deleting in bounded batches rather than one statement matters because the
// writer pool is a single connection (db.SetMaxOpenConns(1)): a DELETE over
// months of rows would hold that connection — and so block every log insert
// and every admin write — for the whole duration. Each batch is its own
// transaction, so a slow prune interleaves with live traffic.
//
// The AFTER DELETE triggers keep the contentless FTS tables in step, so no
// separate index cleanup is needed; that also makes each batch more expensive
// than a bare DELETE, which is another reason to keep them small.
func pruneLogs() (logsDeleted, requestsDeleted int64, err error) {
	if cfg.LogRetentionDays <= 0 {
		return 0, 0, nil
	}

	// RFC3339 in UTC is fixed-width, so a string comparison against the ts
	// column is chronological — no date parsing in SQLite, and the existing
	// idx_logs_ts / idx_request_logs_ts indexes are usable.
	cutoff := time.Now().UTC().AddDate(0, 0, -cfg.LogRetentionDays).Format(time.RFC3339)

	const batchSize = 500
	// maxBatches caps one sweep so a first run against a huge table cannot
	// monopolise the writer indefinitely. The leftovers go on the next tick.
	const maxBatches = 200

	for _, t := range []struct {
		name    string
		deleted *int64
	}{
		{"logs", &logsDeleted},
		{"request_logs", &requestsDeleted},
	} {
		stmt := fmt.Sprintf(
			"DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE ts < ? ORDER BY ts LIMIT %d)",
			t.name, t.name, batchSize)
		for i := 0; i < maxBatches; i++ {
			res, execErr := db.Exec(stmt, cutoff)
			if execErr != nil {
				return logsDeleted, requestsDeleted, fmt.Errorf("prune %s: %w", t.name, execErr)
			}
			n, _ := res.RowsAffected()
			*t.deleted += n
			if n < batchSize {
				break
			}
		}
	}
	return logsDeleted, requestsDeleted, nil
}

// Rate limiter moved to ratelimit.go (in-memory token bucket).
// The legacy rate_limit_entries table is kept in the schema for backwards
// compatibility; it is truncated once on startup and never written to again.

func truncateLegacyRateLimitTable() {
	if _, err := db.Exec("DELETE FROM rate_limit_entries"); err != nil {
		slog.Warn("truncate legacy rate_limit_entries", "err", err)
	}
}
