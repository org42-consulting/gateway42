package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
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
