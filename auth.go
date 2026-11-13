package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	gocrypto "golang.org/x/crypto/pbkdf2"
)

// hashPassword creates a Werkzeug-compatible pbkdf2:sha256 hash.
func hashPassword(password string) string {
	saltBytes := make([]byte, 16)
	rand.Read(saltBytes)
	salt := hex.EncodeToString(saltBytes)
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
	dk := gocrypto.Key([]byte(password), []byte(parts[1]), iterations, 32, sha256.New)
	computed := hex.EncodeToString(dk)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(parts[2])) == 1
}

// generateAPIKey returns a URL-safe random API key (~27 chars).
func generateAPIKey() string {
	b := make([]byte, 20)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ── API key rotation ──────────────────────────────────────────────────────────

// keyIsExpired checks whether a user's API key is past its grace-period
// expiry. An empty key_expires_at means the key has no expiry (normal case).
// A non-empty value is an RFC 3339 timestamp; if now is past it, the key
// is dead even if the row still exists.
func keyIsExpired(expiresAt string) bool {
	if expiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		// Unparseable timestamp: treat as expired (fail closed).
		return true
	}
	return time.Now().After(t)
}

// RotateAPIKey generates a new key for the user and keeps the old one alive
// for a grace period, so a client mid-deployment has time to pick up the new
// key. The old key's hash is stored in api_key_rotations (db.go) with an
// expiry; getUserByAPIKey falls back to that table on a current-key miss,
// which is what makes the old key work during the window.
//
// With graceHours == 0 the old key is revoked immediately: the rotation row
// is written already expired, so it never matches.
//
// The returned key is the new plaintext key — shown to the admin once and
// never stored (only its SHA-256 hash is, via sha256Hex, the same hashing
// used when keys are first created).
func RotateAPIKey(userID int, graceHours int) (newKey string, err error) {
	// Get the current key hash to store in the rotations table.
	var oldHash string
	if err = dbRead.QueryRow("SELECT api_key FROM users WHERE id = ?", userID).Scan(&oldHash); err != nil {
		return "", err
	}

	newPlain := generateAPIKey()
	newHash := sha256Hex(newPlain)
	now := time.Now().UTC()

	// Atomic: the rotation row and the key swap must stand or fall together.
	// A crash between the two would leave either the old key valid with no
	// grace record (harmless) or the new key live while the old hash had no
	// row to explain it (also harmless, but messier to reason about).
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Store the old key with its expiry. An already-past timestamp means
	// "no grace": the row is inert from the moment it is written.
	var expiresAt string
	if graceHours > 0 {
		expiresAt = now.Add(time.Duration(graceHours) * time.Hour).Format(time.RFC3339)
	} else {
		expiresAt = now.Format(time.RFC3339)
	}
	if _, err = tx.Exec(
		"INSERT INTO api_key_rotations (user_id, key_hash, expires_at, created_at) VALUES (?, ?, ?, ?)",
		userID, oldHash, expiresAt, now.Format(time.RFC3339),
	); err != nil {
		return "", err
	}

	// Update the user's current key. key_expires_at is reset to '' as well:
	// a fresh key must not inherit a stale expiry (e.g. one left behind by
	// RevokeOldKey), which would reject the brand-new key on first use.
	if _, err = tx.Exec(
		"UPDATE users SET api_key = ?, key_expires_at = '' WHERE id = ?",
		newHash, userID,
	); err != nil {
		return "", err
	}

	// Clean up expired rotations (housekeeping).
	_, _ = tx.Exec("DELETE FROM api_key_rotations WHERE expires_at < ?", now.Format(time.RFC3339))

	if err = tx.Commit(); err != nil {
		return "", err
	}

	// Drop cached lookups for this user: an entry cached under the old hash
	// would otherwise outlive the grace window and keep a revoked key valid.
	invalidateUserCache(userID)

	return newPlain, nil
}

// RevokeOldKey immediately expires the user's key (sets key_expires_at to
// now), cutting short any rotation grace period. Because keyIsExpired is
// checked on both the current-key path and the rotation path in
// getUserByAPIKey, this stops old in-grace keys at once; it also rejects the
// current key until a rotation installs a fresh one (RotateAPIKey resets
// key_expires_at).
func RevokeOldKey(userID int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec("UPDATE users SET key_expires_at = ? WHERE id = ?", now, userID)
	if err == nil {
		// Take effect on the next request, not after the cache TTL.
		invalidateUserCache(userID)
	}
	return err
}

// ── Admin login throttle ──────────────────────────────────────────────────────

// A wrong admin password costs 260,000 PBKDF2 rounds, and the endpoint that
// checks it is the one place in the gateway reachable without a credential.
// Unthrottled, that is both the brute-force surface and a cheap way to spend
// every CPU cycle the host has — an attacker does not need to guess anything to
// make the gateway work as hard as it can.
//
// The guard counts *failed* authentications, so it never throttles a successful
// login, and it stops hashing the moment the count is reached: skipping the
// derive function is the point, not a side effect of it.
//
// Failures are counted since the last success rather than over a sliding time
// window, and there is no decay — a count that never saw a success persists
// until it locks. That is stricter than a windowed limiter would be, and it is
// the cheaper invariant to hold: the guard's job is to bound the derive
// function's cost, which needs no clock to do, and a single mistyped password
// a week is not going to spend five of them.
//
// Scope is global rather than per-client-address, deliberately. Identity here
// would come from clientIP, which prefers X-Forwarded-For — a header the README
// security model already rules out for rate-limit identity because it is
// client-supplied. Keying on it would let an attacker pick a fresh budget per
// request (defeating the limit) while also being able to lock a real admin out
// by naming their address. There is exactly one admin account, so a global
// budget costs nothing in precision that the spoofable key would have bought.
//
// The trade-off is that five deliberate failures can freeze the admin out for
// the lockout. That is the standard cost of throttling a single-account login,
// it self-heals, and it fits the documented scope: public internet traffic is
// expected to be "rate-limited / denied" here, not served.
const (
	loginMaxFailures = 5 // failures since the last success before the lockout engages
	loginBaseLockout = time.Minute
	loginMaxLockout  = 15 * time.Minute
)

type loginGuard struct {
	mu           sync.Mutex
	failures     int
	lockout      time.Duration // 0 until the first breach; doubles per breach after
	blockedUntil time.Time
}

// adminLogin is a pointer rather than a value so that swapping the guard — as a
// test does to isolate itself — reassigns an address instead of copying a mutex.
var adminLogin = &loginGuard{}

// allow reports whether an attempt may be authenticated now, and for how long
// to hold off when it may not. Callers must check this before hashing: the
// guard exists to make the expensive path rare, and verifying first would pay
// for the derive function on every rejected attempt anyway.
func (g *loginGuard) allow(now time.Time) (ok bool, retryAfter time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.blockedUntil) {
		return false, g.blockedUntil.Sub(now)
	}
	return true, 0
}

// failed records a rejected attempt and returns the lockout it triggered, or 0
// if the allowance is not spent yet. The counter resets when a lockout starts,
// so extending a lockout costs a further full allowance of attempts — the
// doubling punishes persistence, not typos.
func (g *loginGuard) failed(now time.Time) (lockout time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures++
	if g.failures < loginMaxFailures {
		return 0
	}
	switch {
	case g.lockout == 0:
		g.lockout = loginBaseLockout
	case g.lockout*2 <= loginMaxLockout:
		g.lockout *= 2
	default:
		g.lockout = loginMaxLockout
	}
	g.blockedUntil = now.Add(g.lockout)
	g.failures = 0
	return g.lockout
}

// succeeded clears the guard, so one correct password undoes any backlog of
// failures — a user who mistyped three times and then got in is not halfway to
// a lockout on their next visit.
func (g *loginGuard) succeeded() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = 0
	g.lockout = 0
	g.blockedUntil = time.Time{}
}

// ── Admin default-password flag ───────────────────────────────────────────────

const defaultAdminPassword = "admin123"

// isDefaultPassword reports whether the stored admin hash corresponds to the
// well-known default. Used at login to decide whether to set the
// must_change_password flag.
func isDefaultPassword(password string) bool {
	return password == defaultAdminPassword
}

// MustChangePassword returns true if the admin account still has the default
// password and hasn't changed it yet. The flag is set to 1 when the admin
// logs in with the default password ("admin123") or when ADMIN_PASSWORD is
// unset and the stored hash matches the default.
func mustChangePassword() bool {
	var flag int
	row := dbRead.QueryRow("SELECT must_change_password FROM admin_user WHERE id = 1")
	if err := row.Scan(&flag); err != nil {
		return false
	}
	return flag == 1
}

// SetMustChangePassword updates the flag. Called on login (set to 1 if default
// password was used) and on successful password change (set to 0).
func SetMustChangePassword(mustChange bool) {
	val := 0
	if mustChange {
		val = 1
	}
	_, err := db.Exec("UPDATE admin_user SET must_change_password = ? WHERE id = 1", val)
	if err != nil {
		slog.Error("set must_change_password", "err", err)
	}
}

func validateName(name string) bool {
	n := strings.TrimSpace(name)
	return len(n) >= 1 && len(n) <= 100
}

// validatePassword checks password complexity using unicode categories instead of regexes.
func validatePassword(password string) (bool, string) {
	if len(password) < 8 {
		return false, "Password must be at least 8 characters"
	}
	if strings.IndexFunc(password, unicode.IsUpper) < 0 {
		return false, "Password must contain at least one uppercase letter"
	}
	if strings.IndexFunc(password, unicode.IsLower) < 0 {
		return false, "Password must contain at least one lowercase letter"
	}
	if strings.IndexFunc(password, unicode.IsDigit) < 0 {
		return false, "Password must contain at least one digit"
	}
	return true, ""
}

// truncateInput caps text at MaxMsgLen bytes, backing off to the previous rune
// boundary so the cut never lands mid-sequence. A byte-exact slice would leave
// a partial UTF-8 rune at the tail, which json.Marshal silently replaces with
// U+FFFD in the audit log rather than rejecting.
func truncateInput(text string) string {
	if len(text) <= cfg.MaxMsgLen {
		return text
	}
	cut := cfg.MaxMsgLen
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
