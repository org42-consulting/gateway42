// Package ratelimit provides the per-user request rate limiter.
//
// It is deliberately free of Gateway42 globals: a limiter instance owns only
// its buckets, so it can be constructed, swapped and tested in isolation, and
// a distributed backend (Redis, etc.) can be substituted for multi-instance
// deployments without touching handlers or middleware.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// In-memory per-user rate limiter. Replaces the DB-backed table-scan
// implementation that did DELETE+SELECT+INSERT on every API call.
//
// Each user gets a token-bucket sized so that:
//   - sustained rate = ratePerMinute requests / 60s
//   - short bursts up to ratePerMinute are allowed
//
// Idle limiters (no use for IdleEvictAfter) are reaped by a background
// sweeper so the map cannot grow without bound.

// RateLimiter decides whether a user may make another request right now.
// The in-memory token bucket is the default implementation; the interface
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

// TokenBucketLimiter is the in-memory RateLimiter. One process, one set of
// buckets — running multiple instances behind a load balancer multiplies
// the effective limit by the instance count (documented constraint).
type TokenBucketLimiter struct {
	mu sync.Mutex
	m  map[int]*userLimiter
}

// NewTokenBucketLimiter returns a fresh in-memory limiter with no buckets.
func NewTokenBucketLimiter() RateLimiter {
	return &TokenBucketLimiter{m: make(map[int]*userLimiter)}
}

// Allow reports whether a request from userID is within their per-minute
// limit, consuming one token when true. Safe for concurrent use across many
// goroutines.
func (tb *TokenBucketLimiter) Allow(userID, ratePerMinute int) bool {
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
func (tb *TokenBucketLimiter) Forget(userID int) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	delete(tb.m, userID)
}

// ResetAll discards every bucket, so the next request from each user starts
// from a full burst allowance. It is an in-memory detail: a substituted
// distributed backend would define its own reset semantics, which is why it
// lives on the concrete type rather than the RateLimiter interface.
func (tb *TokenBucketLimiter) ResetAll() {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.m = make(map[int]*userLimiter)
}

// SweepIdle evicts entries unused for IdleEvictAfter. Like ResetAll it is an
// in-memory detail exposed on the concrete type.
func (tb *TokenBucketLimiter) SweepIdle() {
	cutoff := time.Now().Add(-IdleEvictAfter)
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

// IdleEvictAfter is how long a bucket may sit unused before the sweeper drops
// it.
const IdleEvictAfter = 30 * time.Minute
