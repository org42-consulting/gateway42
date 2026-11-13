package main

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// In-memory per-user rate limiter. Replaces the DB-backed table-scan
// implementation that did DELETE+SELECT+INSERT on every API call.
//
// Each user gets a token-bucket sized so that:
//   - sustained rate = user.RateLimit requests / 60s
//   - short bursts up to user.RateLimit are allowed
//
// Idle limiters (no use for idleEvictAfter) are reaped by a background
// sweeper so the map cannot grow without bound.

// RateLimiter decides whether a user may make another request right now.
// The in-memory token bucket is the only implementation; the interface
// exists so a distributed backend (Redis, etc.) can be substituted for
// multi-instance deployments without touching handlers or middleware.
type RateLimiter interface {
	// Allow reports whether the user may proceed. It consumes one token
	// when returning true. When false, the caller should reject with 429.
	Allow(userID int, ratePerMinute int) bool

	// Forget removes the user's bucket. Called when a user is deleted (or
	// their limit changes) so their state doesn't linger.
	Forget(userID int)
}

// tokenBucketLimiter is the in-memory RateLimiter. One process, one set of
// buckets — running multiple instances behind a load balancer multiplies
// the effective limit by the instance count (documented constraint).
type tokenBucketLimiter struct {
	mu sync.Mutex
	m  map[int]*userLimiter
}

func newRateLimiter() RateLimiter {
	return &tokenBucketLimiter{m: make(map[int]*userLimiter)}
}

// Allow reports whether a request from userID is within their per-minute
// limit, consuming one token when true. Safe for concurrent use across many
// goroutines.
func (tb *tokenBucketLimiter) Allow(userID, ratePerMinute int) bool {
	if ratePerMinute <= 0 {
		// A non-positive limit denies all requests (e.g. an admin has
		// disabled a user). Checking up front also skips creating a bucket
		// that could never hold any tokens.
		return false
	}
	tb.mu.Lock()
	now := time.Now()
	entry, ok := tb.m[userID]
	if !ok || entry.limit != ratePerMinute {
		// (re)create on first use or when admin changes the per-user limit.
		l := rate.NewLimiter(rate.Limit(float64(ratePerMinute)/60.0), ratePerMinute)
		entry = &userLimiter{limiter: l, limit: ratePerMinute, lastUse: now}
		tb.m[userID] = entry
	}
	entry.lastUse = now
	tb.mu.Unlock()

	// Allow runs outside the registry lock on purpose. rate.Limiter has its
	// own mutex, so holding both meant every API request in the process —
	// for every user — queued through one global lock twice. The entry is a
	// pointer, so it stays valid after the unlock even if the sweeper or a
	// limit change drops it from the map meanwhile.
	return entry.limiter.Allow()
}

// Forget removes the user's bucket.
func (tb *tokenBucketLimiter) Forget(userID int) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	delete(tb.m, userID)
}

// resetAll discards every bucket; see resetAllLimiters for why that exists.
func (tb *tokenBucketLimiter) resetAll() {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.m = make(map[int]*userLimiter)
}

// sweep evicts entries unused for idleEvictAfter.
func (tb *tokenBucketLimiter) sweep() {
	cutoff := time.Now().Add(-idleEvictAfter)
	tb.mu.Lock()
	defer tb.mu.Unlock()
	for id, e := range tb.m {
		if e.lastUse.Before(cutoff) {
			delete(tb.m, id)
		}
	}
}

// userLimiter holds one user's bucket plus the bookkeeping needed to rebuild
// it when their limit changes and to evict it when it goes idle.
type userLimiter struct {
	limiter *rate.Limiter
	limit   int       // last-known per-minute limit; rebuilt if changed
	lastUse time.Time // for idle eviction
}

// idleEvictAfter is how long a bucket may sit unused before the sweeper drops
// it.
const idleEvictAfter = 30 * time.Minute

// Package-level default instance. Middleware and handlers use this;
// tests can swap it for a fake.
var defaultLimiter RateLimiter = newRateLimiter()

// dropUserLimiter removes a user's limiter (admin deleted the user or
// changed their rate limit).
func dropUserLimiter(userID int) {
	defaultLimiter.Forget(userID)
}

// resetAllLimiters discards every in-memory bucket, so the next request from
// each user starts from a full burst allowance. Called by the admin "Reset
// System" action, whose label promises exactly that — before this existed the
// button only truncated a legacy table that nothing writes to, and live
// throttling survived the reset untouched.
//
// Note the trade-off: a client currently being throttled is immediately free
// to burst again, which is the point of a manual reset but also means it can
// be used to wave a misbehaving client through.
//
// The reset is an in-memory detail, so it goes through a type assertion: a
// substituted distributed backend would define its own reset semantics.
func resetAllLimiters() {
	if tb, ok := defaultLimiter.(*tokenBucketLimiter); ok {
		tb.resetAll()
	}
}

// sweepIdleLimiters evicts entries unused for idleEvictAfter. It runs on the
// background ticker in main.go; like resetAllLimiters it is an in-memory
// detail exposed through a type assertion.
func sweepIdleLimiters() {
	if tb, ok := defaultLimiter.(*tokenBucketLimiter); ok {
		tb.sweep()
	}
}
