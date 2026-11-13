package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The daily token quota gates total consumption (tokens since UTC midnight,
// summed over both completion paths) rather than speed. It is enforced in both
// handleChatCompletions and handleMessages — the Anthropic route was a
// previously-missed gap, so it gets its own test — and it fails open when the
// read pool cannot answer: a quota check that blocks all traffic on a DB
// hiccup is worse than a slightly over-budget user.
//
// withTestDB opens the database through initDB, which runs the migration that
// adds users.quota_daily_tokens in db.go — so every test here exercises the
// post-migration state, with no need to add the column by hand.

// qtRequireQuotaColumn asserts that migration ran. It pins the real bug this
// column guards against: while the column was missing, checkQuota failed open
// silently and SetQuota errored on every write, so quota enforcement never
// actually engaged on a fresh database.
func qtRequireQuotaColumn(t *testing.T) {
	t.Helper()
	var haveQuotaCol int
	if err := dbRead.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'quota_daily_tokens'`,
	).Scan(&haveQuotaCol); err != nil || haveQuotaCol == 0 {
		t.Fatalf("users.quota_daily_tokens missing — the db.go migration did not run")
	}
}

// qtSeedUser creates an active user with a daily token quota (0 = unlimited).
func qtSeedUser(t *testing.T, name, key string, rateLimit, quota int) {
	t.Helper()
	if err := createUser(name, key, rateLimit); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if _, err := db.Exec("UPDATE users SET status='active', quota_daily_tokens=? WHERE name=?", quota, name); err != nil {
		t.Fatalf("set quota: %v", err)
	}
}

// qtSeedUsage records one completed request for name on path, counting as
// in+out tokens consumed today (the quota's SUM window).
func qtSeedUsage(t *testing.T, name, path string, in, out int) {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		"INSERT INTO request_logs(ts, method, path, client_ip, user_name, status_code, in_tokens, out_tokens) VALUES(?,?,?,?,?,?,?,?)",
		ts, http.MethodPost, path, "127.0.0.1", name, http.StatusOK, in, out); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

// qtDrive runs one request through the full production router.
func qtDrive(t *testing.T, method, target, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	setupRouter().ServeHTTP(rec, r)
	return rec
}

// A user over their daily budget is a 429 before the body is even read.
func TestQuotaDailyExceededReturns429(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	qtRequireQuotaColumn(t)
	qtSeedUser(t, "qt-exceeded", "sk-gw-qt-exceeded", 60, 10)
	qtSeedUsage(t, "qt-exceeded", "/v1/chat/completions", 6, 6) // 12 > 10

	rec := qtDrive(t, http.MethodPost, "/v1/chat/completions", "sk-gw-qt-exceeded",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d %s, want 429 with 12 of 10 tokens used", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `"message":"Daily token quota exceeded"`) {
		t.Errorf("429 body missing the quota message: %s", got)
	}
	if !strings.Contains(rec.Body.String(), `"type":"rate_limit_error"`) {
		t.Errorf("429 body missing rate_limit_error type: %s", rec.Body.String())
	}
}

// Under the budget the request passes admission; with no engines configured
// it dies later at routing (404), which is what "allowed" looks like here.
func TestQuotaUnderLimitIsAllowed(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	qtRequireQuotaColumn(t)
	qtSeedUser(t, "qt-under", "sk-gw-qt-under", 60, 10)
	qtSeedUsage(t, "qt-under", "/v1/chat/completions", 3, 2) // 5 of 10

	rec := qtDrive(t, http.MethodPost, "/v1/chat/completions", "sk-gw-qt-under",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("status = 429 with 5 of 10 tokens used; under-quota requests must be allowed")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d %s, want 404 (admitted, then no engine for the model)", rec.Code, rec.Body.String())
	}
}

// quota_daily_tokens = 0 means unlimited: usage past any budget is ignored.
func TestQuotaZeroMeansUnlimited(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	qtRequireQuotaColumn(t)
	qtSeedUser(t, "qt-unlimited", "sk-gw-qt-unlimited", 60, 0)
	qtSeedUsage(t, "qt-unlimited", "/v1/chat/completions", 50, 50)

	rec := qtDrive(t, http.MethodPost, "/v1/chat/completions", "sk-gw-qt-unlimited",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("status = 429 for an unlimited user; quota 0 must mean no cap")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d %s, want 404 (admitted)", rec.Code, rec.Body.String())
	}
}

// The Anthropic path carries the same gate: checkQuota sums consumption from
// both completion paths, so a Claude Code client that only calls /v1/messages
// must still hit its quota. This was the previously-missed gap.
func TestQuotaEnforcedOnMessagesRoute(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	qtRequireQuotaColumn(t)
	qtSeedUser(t, "qt-anthropic", "sk-gw-qt-anthropic", 60, 10)
	qtSeedUsage(t, "qt-anthropic", "/v1/messages", 6, 6) // 12 > 10

	rec := qtDrive(t, http.MethodPost, "/v1/messages", "sk-gw-qt-anthropic",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d %s, want 429 — /v1/messages must be gated by the quota", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `"message":"Daily token quota exceeded"`) {
		t.Errorf("429 body missing the quota message: %s", got)
	}
	if !strings.HasPrefix(rec.Body.String(), `{"type":"error","error":{"type":"rate_limit_error"`) {
		t.Errorf("429 body is not the Anthropic envelope: %s", rec.Body.String())
	}
}

// Fail-open: when the quota check cannot read the DB, the request is allowed.
// The first request (live reader) proves the gate is active and, as a side
// effect, caches the user; the reader is then broken the way a transient DB
// failure looks to checkQuota, and the second request must pass admission
// instead of being blocked — 404 at routing, not 429 at the quota.
func TestQuotaFailsOpenWhenReaderIsBroken(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	qtRequireQuotaColumn(t)
	qtSeedUser(t, "qt-failopen", "sk-gw-qt-failopen", 60, 10)
	qtSeedUsage(t, "qt-failopen", "/v1/chat/completions", 6, 6) // over the limit

	rec := qtDrive(t, http.MethodPost, "/v1/chat/completions", "sk-gw-qt-failopen",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("first request = %d, want 429 — the gate must be active on a live reader", rec.Code)
	}

	breakReads(t) // close dbRead: checkQuota's queries now error

	rec = qtDrive(t, http.MethodPost, "/v1/chat/completions", "sk-gw-qt-failopen",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("a quota check that cannot read the DB blocked the request; it must fail open")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d %s, want 404 (allowed past the quota, then no engine)", rec.Code, rec.Body.String())
	}
}
