package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The request log used to answer "who called what when". These pin the detail
// columns: the routing, timing and token facts handlers now hand to the log,
// the correlation header the client sees, and the migration that gives an
// existing database the new shape.

func TestRequestDetailFlowsToLogEntry(t *testing.T) {
	drainRequestLog()

	// A handler standing in for the real path: route, stream a first token,
	// record usage.
	h := requestLoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := detailFromContext(r)
		if d == nil {
			t.Fatal("handler has no detail collector")
		}
		d.setRouted("omlx", "openai-compat", "qwen3")
		d.setStreamed()
		d.setFirstToken()
		d.applyMeta(chunkMeta{model: "qwen3:8b", finish: "length", inTokens: 120, outTokens: 340})
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "claude-cli/2.0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	entries := drainRequestLog()
	if len(entries) != 1 {
		t.Fatalf("got %d log entries, want 1", len(entries))
	}
	e := entries[0]
	d := e.detail
	if d == nil {
		t.Fatal("log entry has no detail")
	}
	if !strings.HasPrefix(d.id, "req_") {
		t.Errorf("request id = %q, want req_ prefix", d.id)
	}
	if got := rec.Header().Get("X-Request-ID"); got != d.id {
		t.Errorf("X-Request-ID = %q, want the row's id %q", got, d.id)
	}
	if d.engineName != "omlx" || d.engineType != "openai-compat" || d.modelRequested != "qwen3" {
		t.Errorf("routing = %s/%s/%s", d.engineName, d.engineType, d.modelRequested)
	}
	if d.modelServed != "qwen3:8b" {
		t.Errorf("model served = %q, want qwen3:8b", d.modelServed)
	}
	if !d.streamed || d.stopReason != "length" || d.inTokens != 120 || d.outTokens != 340 {
		t.Errorf("stream facts = %+v", d)
	}
	if d.ttft == 0 || d.duration == 0 {
		t.Errorf("ttft=%v duration=%v, want both measured", d.ttft, d.duration)
	}
	if d.userAgent != "claude-cli/2.0" {
		t.Errorf("user agent = %q", d.userAgent)
	}
}

// A request that never reaches a handler (auth rejection, panic) still gets a
// row — the detail is empty, not missing.
func TestRequestDetailAbsentStillLogs(t *testing.T) {
	drainRequestLog()
	h := requestLoggingMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	entries := drainRequestLog()
	if len(entries) != 1 || entries[0].detail == nil {
		t.Fatalf("entries = %+v, want one with a detail", entries)
	}
}

func TestFlushRequestBatchWritesDetails(t *testing.T) {
	withTestDB(t)

	d := &requestDetail{
		id: "req_test1234", start: time.Now().Add(-2 * time.Second),
		duration: 1500 * time.Millisecond, ttft: 400 * time.Millisecond,
		modelRequested: "qwen3", modelServed: "qwen3:8b",
		engineName: "omlx", engineType: "openai-compat",
		streamed: true, stopReason: "stop", inTokens: 11, outTokens: 5,
		outcome: "client disconnected", upstreamErr: "connection reset",
		userAgent: "claude-cli/2.0",
	}
	flushRequestBatch([]requestLogEntry{{
		ts: time.Now().UTC().Format(time.RFC3339), method: "POST",
		path: "/v1/messages", clientIP: "127.0.0.1", userName: "u", status: 200,
		detail: d,
	}})
	// A nil-detail entry is the pre-collector shape; it must still insert.
	flushRequestBatch([]requestLogEntry{{
		ts: time.Now().UTC().Format(time.RFC3339), method: "GET",
		path: "/v1/models", clientIP: "127.0.0.1", userName: "u", status: 200,
	}})

	rows, err := dbRead.Query(`SELECT ` + requestLogColumns + ` FROM request_logs ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []RequestLogRow
	for rows.Next() {
		var r RequestLogRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Method, &r.Path, &r.ClientIP, &r.UserName, &r.StatusCode,
			&r.RequestID, &r.ModelRequested, &r.ModelServed, &r.EngineName, &r.EngineType,
			&r.Streamed, &r.TTFTMs, &r.DurationMs, &r.InTokens, &r.OutTokens,
			&r.StopReason, &r.Outcome, &r.UpstreamError, &r.UserAgent); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	r := got[0]
	if r.RequestID != "req_test1234" || r.ModelServed != "qwen3:8b" || r.EngineName != "omlx" {
		t.Errorf("detail columns = %+v", r)
	}
	if r.Streamed != 1 || r.TTFTMs != 400 || r.DurationMs != 1500 || r.InTokens != 11 || r.OutTokens != 5 {
		t.Errorf("numeric detail = %+v", r)
	}
	if r.StopReason != "stop" || r.Outcome != "client disconnected" ||
		r.UpstreamError != "connection reset" || r.UserAgent != "claude-cli/2.0" {
		t.Errorf("text detail = %+v", r)
	}
	if got[1].RequestID != "" || got[1].DurationMs != 0 {
		t.Errorf("nil-detail row = %+v, want zero values", got[1])
	}
}

// An existing database predates the detail columns; initDB must add them and
// rebuild the contentless FTS table (which cannot gain columns in place) so
// model and engine search works on old rows too.
func TestRequestLogsMigrationAddsDetails(t *testing.T) {
	prevPath, prevDB, prevRead := cfg.DBPath, db, dbRead
	t.Cleanup(func() {
		if dbRead != nil {
			_ = dbRead.Close()
		}
		if db != nil {
			_ = db.Close()
		}
		cfg.DBPath, db, dbRead = prevPath, prevDB, prevRead
	})

	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE request_logs(
			id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, method TEXT NOT NULL,
			path TEXT NOT NULL, client_ip TEXT NOT NULL, user_name TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL)`,
		`INSERT INTO request_logs(ts, method, path, client_ip, user_name, status_code)
			VALUES('2026-01-01T00:00:00Z','POST','/v1/chat/completions','1.2.3.4','olduser',200)`,
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	raw.Close()

	cfg.DBPath = path
	if err := initDB(); err != nil {
		t.Fatalf("initDB on old schema: %v", err)
	}

	for _, col := range []string{"request_id", "model_served", "engine_name", "duration_ms", "user_agent"} {
		var found int
		err := dbRead.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_logs') WHERE name=?`, col).Scan(&found)
		if err != nil || found != 1 {
			t.Errorf("column %s missing after migration (err=%v)", col, err)
		}
	}
	// The old row survives with defaults.
	var status int
	var requestID string
	if err := dbRead.QueryRow(
		`SELECT status_code, request_id FROM request_logs WHERE user_name='olduser'`).
		Scan(&status, &requestID); err != nil {
		t.Fatalf("old row unreadable: %v", err)
	}
	if status != 200 || requestID != "" {
		t.Errorf("old row = %d/%q, want 200/empty", status, requestID)
	}
	// FTS rebuilt with the new columns and backfilled: the old row is findable.
	if fts5Enabled {
		var n int
		if err := dbRead.QueryRow(
			`SELECT COUNT(*) FROM request_logs_fts WHERE request_logs_fts MATCH 'olduser'`).Scan(&n); err != nil {
			t.Fatalf("fts query after migration: %v", err)
		}
		if n != 1 {
			t.Errorf("fts found %d rows for 'olduser', want 1 (backfill missed the old row)", n)
		}
	}
}
