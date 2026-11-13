package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────── Token quotas ────────────────────────────────
//
// Quotas bound total consumption (tokens), not speed (requests/minute). A user
// with a 100k token/day quota can burst at their rate limit but stops when the
// budget is spent, regardless of how slowly they use it. This complements rate
// limiting: rate protects hardware from bursts, quota protects the operator
// from a user (or a buggy agent loop) burning through all inference capacity
// over a long period.
//
// Quotas are per-user, per-period (daily). The period resets at UTC midnight.
// A quota of 0 means unlimited (the default for new users).
//
// Enforcement reads the same request_logs table the usage panel aggregates —
// no separate counter, no new write on the request path beyond what already
// happens. The check is a single indexed SUM query on the read pool.
//
// The users.quota_daily_tokens column is added by the db.go migration. A
// database that somehow predates it (initDB runs at every startup, so this is
// defensive only) reads as unlimited: queries against the missing column
// error, checkQuota fails open on that error, and the dashboard panel renders
// nothing.

// QuotaStatus reports a user's quota state for the current period.
type QuotaStatus struct {
	Limit     int    // 0 = unlimited
	Used      int    // tokens consumed this period
	Remaining int    // Limit - Used, or -1 if unlimited
	Period    string // "daily" (future: "weekly")
}

// unlimitedQuota is the state a user is in whenever no positive limit applies:
// quota not set, user unknown, or the check itself could not run.
func unlimitedQuota() QuotaStatus {
	return QuotaStatus{Limit: 0, Used: 0, Remaining: -1, Period: "daily"}
}

// checkQuota returns the user's quota status. It is called on the request path
// before allowing a completion. If the user has exceeded their quota, the
// handler should reject with 429 and a "quota exceeded" message.
//
// The query is on the read pool (dbRead) and hits the idx_request_logs_ts
// index. For a typical user (thousands of rows per day) this is sub-millisecond.
//
// Every failure mode fails open (allow the request) rather than closed: a
// quota check that blocks all traffic on a DB hiccup is worse than a slightly
// over-budget user. The one failure that must not become log noise is the
// pre-migration state, where the column does not exist yet and this would
// warn on every single request.
func checkQuota(userName string) QuotaStatus {
	// Get the user's quota limit.
	var limit int
	err := dbRead.QueryRow(
		"SELECT quota_daily_tokens FROM users WHERE name = ?",
		userName,
	).Scan(&limit)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && !isMissingColumn(err) {
			slog.Warn("quota limit lookup failed, allowing request", "user", userName, "err", err)
		}
		// No quota set, user not found, or the column has not been migrated
		// in yet: treat as unlimited.
		return unlimitedQuota()
	}
	if limit == 0 {
		return unlimitedQuota()
	}

	// Sum tokens consumed since UTC midnight. ts is stored as fixed-width
	// RFC3339 UTC (see getUsageByClient), so a text cutoff compares
	// chronologically and the index serves the range. The path filter matches
	// the usage panel: /v1/models-style paths carry zero tokens and would
	// only dilute the window.
	midnight := time.Now().UTC().Truncate(24 * time.Hour)
	var used int
	err = dbRead.QueryRow(
		"SELECT COALESCE(SUM(in_tokens + out_tokens), 0) FROM request_logs WHERE user_name = ? AND ts >= ? AND path IN ('/v1/chat/completions', '/v1/messages')",
		userName, midnight.Format(time.RFC3339),
	).Scan(&used)
	if err != nil {
		// Query failure: fail open (allow the request) and log. A quota
		// check that blocks all traffic on a DB hiccup is worse than a
		// slightly over-budget user.
		slog.Warn("quota check failed, allowing request", "user", userName, "err", err)
		return QuotaStatus{Limit: limit, Used: 0, Remaining: -1, Period: "daily"}
	}

	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return QuotaStatus{Limit: limit, Used: used, Remaining: remaining, Period: "daily"}
}

// isMissingColumn reports whether the error is SQLite's "no such column",
// the pre-migration state of users.quota_daily_tokens. It is string-matched
// rather than typed because the point is to recognise one specific, transient
// schema state across driver versions, and the gateway is sqlite-only.
func isMissingColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such column")
}

// quotaExceeded reports whether the user should be rejected.
func (q QuotaStatus) quotaExceeded() bool {
	return q.Limit > 0 && q.Remaining <= 0
}

// SetQuota updates a user's daily token quota. 0 = unlimited.
// Called from handleSetQuota in handlers.go (admin UI).
func SetQuota(userName string, dailyTokens int) error {
	if dailyTokens < 0 {
		return fmt.Errorf("quota cannot be negative")
	}
	_, err := db.Exec("UPDATE users SET quota_daily_tokens = ? WHERE name = ?", dailyTokens, userName)
	return err
}

// GetQuota returns the user's current daily token quota limit.
func GetQuota(userName string) (int, error) {
	var limit int
	err := dbRead.QueryRow("SELECT quota_daily_tokens FROM users WHERE name = ?", userName).Scan(&limit)
	return limit, err
}

// ─────────────────────────────── Dashboard quota display ──────────────────────

// QuotaRow is one user's quota state for the dashboard table.
type QuotaRow struct {
	Name      string
	Limit     int
	Used      int
	Remaining int
	Bar       string // quantized qb-* class, see quotaBarClass
}

// quotaBarClass maps a user's consumed fraction of the daily budget to one of
// eleven fixed widths, the same quantized-class trick as usageBarClass:
// static_css_test forbids style attributes in templates, and a data-driven
// width would have to be a style attribute, so Go picks the class and the
// stylesheet already knows every width.
//
// Unlike usageBarClass there is no 10% floor for the smallest non-zero value.
// That floor exists because usage bars rank clients against each other — the
// smallest spender still has to be visibly present. A quota bar is an
// absolute gauge of one user's budget, where the fraction itself is the
// signal: 3% consumed is 97% fine, and a bar pinned at 10% would read as a
// problem that does not exist.
func quotaBarClass(used, limit int) string {
	if limit <= 0 {
		return "qb-0"
	}
	pct := used * 100 / limit
	pct -= pct % 10 // floor to the class grid; a gauge must never overstate
	if pct > 100 {
		pct = 100
	}
	return "qb-" + strconv.Itoa(pct)
}

// buildQuotaRows assembles the quota panel data for the dashboard.
// One query per user (the dashboard has at most a few dozen users).
// Wired into DashboardData.QuotaRows by handleAdminPanel (handlers.go).
//
// The result is nil — not an empty slice — when there is nothing to show,
// which is what the template's {{if .QuotaRows}} tests: unlimited users do
// not get rows, so a fully unlimited fleet renders no panel at all.
func buildQuotaRows(users []User) []QuotaRow {
	// users.quota_daily_tokens is added by the db.go migration. Probe the
	// schema once up front so a database that predates it reads as "panel
	// not applicable" instead of one SQL error per user per page load. The pragma_table_info call is the codebase's own column-check
	// pattern (see request_detail_test).
	var haveQuotaCol int
	if err := dbRead.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'quota_daily_tokens'`,
	).Scan(&haveQuotaCol); err != nil || haveQuotaCol == 0 {
		return nil
	}

	midnight := time.Now().UTC().Truncate(24 * time.Hour)
	rows := make([]QuotaRow, 0, len(users))
	for _, u := range users {
		limit := 0
		if err := dbRead.QueryRow(
			"SELECT quota_daily_tokens FROM users WHERE id = ?",
			u.ID,
		).Scan(&limit); err != nil {
			// The column exists (probed above), so this is a real DB hiccup.
			// Skip the row rather than fail the page: a dashboard degrades to
			// fewer rows, not to an error screen.
			slog.Warn("quota limit lookup failed, skipping user", "user", u.Name, "err", err)
			continue
		}
		if limit == 0 {
			continue // unlimited users don't show in the quota panel
		}

		var used int
		if err := dbRead.QueryRow(
			"SELECT COALESCE(SUM(in_tokens + out_tokens), 0) FROM request_logs WHERE user_name = ? AND ts >= ? AND path IN ('/v1/chat/completions', '/v1/messages')",
			u.Name, midnight.Format(time.RFC3339),
		).Scan(&used); err != nil {
			// Same fail-open philosophy as checkQuota, minus the pressure:
			// the panel is not on the request path, so logging is cheap and
			// showing a bogus "0 used" would be worse than omitting the row.
			slog.Warn("quota usage lookup failed, skipping user", "user", u.Name, "err", err)
			continue
		}

		remaining := limit - used
		if remaining < 0 {
			remaining = 0
		}
		rows = append(rows, QuotaRow{
			Name:      u.Name,
			Limit:     limit,
			Used:      used,
			Remaining: remaining,
			Bar:       quotaBarClass(used, limit),
		})
	}
	return rows
}
