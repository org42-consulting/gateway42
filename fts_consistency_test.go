package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// Tests for checkFTSConsistency (db.go): the contentless FTS5 indexes must
// stay row-for-row in count with their source tables, and the check must
// notice when they drift.

// ftsRows reads a table's row count through the reader pool.
func ftsRows(t *testing.T, table string) int64 {
	t.Helper()
	var n int64
	if err := dbRead.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// withLogCapture routes the default slog logger to a buffer for the duration
// of the test and restores the previous logger on cleanup. checkFTSConsistency
// reports through slog only, so the log is the only observation point it has.
func withLogCapture(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(&logCaptureHandler{buf: &buf}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type logCaptureHandler struct{ buf *strings.Builder }

func (h *logCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *logCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.buf.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		h.buf.WriteByte(' ')
		h.buf.WriteString(a.Key)
		h.buf.WriteByte('=')
		h.buf.WriteString(a.Value.String())
		return true
	})
	h.buf.WriteByte('\n')
	return nil
}

func (h *logCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logCaptureHandler) WithGroup(string) slog.Handler      { return h }

var ftsPairs = [][2]string{
	{"logs", "logs_fts"},
	{"request_logs", "request_logs_fts"},
}

// seedFTSRows puts one row in each source table through the same INSERT that
// the batched writer (logwriter.go) issues, which is what fires the AFTER
// INSERT trigger keeping each FTS index in step.
func seedFTSRows(t *testing.T, userName string) (uid int) {
	t.Helper()
	if err := createUser(userName, "k-"+userName, 10); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if err := db.QueryRow("SELECT id FROM users WHERE name = ?", userName).Scan(&uid); err != nil {
		t.Fatalf("lookup user: %v", err)
	}
	ts := "2026-01-01T00:00:00Z"
	if _, err := db.Exec(
		"INSERT INTO logs(user_id, model, prompt, response, ts) VALUES (?,?,?,?,?)",
		uid, "qwen", "alpha prompt", "alpha response", ts); err != nil {
		t.Fatalf("insert log row: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO request_logs(ts, method, path, client_ip, user_name, status_code) VALUES (?,?,?,?,?,?)",
		ts, "POST", "/v1/messages", "10.0.0.9", userName, 200); err != nil {
		t.Fatalf("insert request log row: %v", err)
	}
	return uid
}

// Normal case: with the triggers doing their job the FTS row count equals the
// source row count, and the consistency check leaves a consistent database
// completely alone — no rebuild, no drift warning.
func TestFTSConsistencyCleanOnConsistentDatabase(t *testing.T) {
	withTestDB(t)
	logs := withLogCapture(t)
	seedFTSRows(t, "fts-alice")

	for _, p := range ftsPairs {
		if main, fts := ftsRows(t, p[0]), ftsRows(t, p[1]); main != 1 || fts != main {
			t.Fatalf("%s: main=%d fts=%d before the check, want both 1 (trigger did not fire)", p[0], main, fts)
		}
	}

	checkFTSConsistency()

	for _, p := range ftsPairs {
		if main, fts := ftsRows(t, p[0]), ftsRows(t, p[1]); fts != main {
			t.Errorf("%s: main=%d fts=%d after the check, want equal", p[0], main, fts)
		}
	}
	if out := logs.String(); strings.Contains(out, "out of sync") || strings.Contains(out, "rebuild") {
		t.Errorf("a consistent database was touched:\n%s", out)
	}
}

// Drift case: an FTS entry is removed directly from the contentless table —
// bypassing the trigger, the way a trigger that failed mid-transaction would
// leave it — so the FTS count no longer matches the source. The check must
// detect the drift in both indexes and repair it: contentless FTS5 tables
// reject the 'rebuild' command, so the repair is a 'delete-all' plus a
// backfill from the source table (the same statement createSchema uses),
// after which the index matches the source again and serves MATCH queries
// from the restored rows.
func TestFTSConsistencyDetectsDrift(t *testing.T) {
	withTestDB(t)
	logs := withLogCapture(t)
	seedFTSRows(t, "fts-bob")

	var logID, reqID int
	if err := db.QueryRow("SELECT id FROM logs").Scan(&logID); err != nil {
		t.Fatalf("log id: %v", err)
	}
	if err := db.QueryRow("SELECT id FROM request_logs").Scan(&reqID); err != nil {
		t.Fatalf("request log id: %v", err)
	}

	// A contentless FTS5 table has no row to DELETE, so the drift is forced
	// with the contentless 'delete' command and the original column values —
	// the same mechanism the AFTER DELETE trigger uses, aimed at a row that
	// is still present in the source table.
	if _, err := db.Exec(
		"INSERT INTO logs_fts(logs_fts, rowid, prompt, response, user_name) VALUES ('delete', ?, 'alpha prompt', 'alpha response', 'fts-bob')",
		logID); err != nil {
		t.Fatalf("drift logs_fts: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO request_logs_fts(request_logs_fts, rowid, path, client_ip, user_name, model_served, engine_name) VALUES ('delete', ?, '/v1/messages', '10.0.0.9', 'fts-bob', '', '')",
		reqID); err != nil {
		t.Fatalf("drift request_logs_fts: %v", err)
	}
	for _, p := range ftsPairs {
		if main, fts := ftsRows(t, p[0]), ftsRows(t, p[1]); fts == main {
			t.Fatalf("%s: main=%d fts=%d, drift was not injected", p[0], main, fts)
		}
	}

	checkFTSConsistency()

	out := logs.String()
	for _, p := range ftsPairs {
		if !strings.Contains(out, "FTS index out of sync") || !strings.Contains(out, "table="+p[0]) {
			t.Errorf("drift in %s was not detected (log:\n%s)", p[0], out)
		}
	}

	for _, p := range ftsPairs {
		if !strings.Contains(out, "FTS index repaired") || !strings.Contains(out, "table="+p[0]) {
			t.Errorf("repair of %s was not reported as successful (log:\n%s)", p[0], out)
		}
		// The check must not touch the source table while repairing.
		if n := ftsRows(t, p[0]); n != 1 {
			t.Errorf("%s: main=%d after the check, want 1 (source must be untouched)", p[0], n)
		}
		// The index is back in step with the source.
		if main, fts := ftsRows(t, p[0]), ftsRows(t, p[1]); fts != main {
			t.Errorf("%s: main=%d fts=%d after the repair, want equal", p[0], main, fts)
		}
	}

	// The repaired index serves searches again: a MATCH on restored content
	// finds exactly the one row in each index.
	for _, row := range []struct {
		table, term string
		want        int64
	}{
		{"logs_fts", "alpha", 1},
		{"request_logs_fts", "messages", 1},
	} {
		var n int64
		if err := dbRead.QueryRow(
			"SELECT COUNT(*) FROM "+row.table+" WHERE "+row.table+" MATCH ?", row.term,
		).Scan(&n); err != nil {
			t.Fatalf("match %s: %v", row.table, err)
		}
		if n != row.want {
			t.Errorf("%s MATCH %q: rows=%d, want %d (repaired index does not serve searches)", row.table, row.term, n, row.want)
		}
	}
}
