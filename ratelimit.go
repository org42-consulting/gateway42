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

const idleEvictAfter = 30 * time.Minute

type userLimiter struct {
	limiter *rate.Limiter
	limit   int       // last-known per-minute limit; rebuilt if changed
	lastUse time.Time // for idle eviction
}

type rateLimiterRegistry struct {
	mu sync.Mutex
	m  map[int]*userLimiter
}

var rlReg = &rateLimiterRegistry{m: make(map[int]*userLimiter)}

// isAllowed returns true if a request from userID is within their per-minute
// limit. Safe for concurrent use across many goroutines.
func isAllowed(userID, limit int) bool {
	if limit <= 0 {
		return false
	}
	rlReg.mu.Lock()
	defer rlReg.mu.Unlock()

	now := time.Now()
	entry, ok := rlReg.m[userID]
	if !ok || entry.limit != limit {
		// (re)create on first use or when admin changes the per-user limit.
		l := rate.NewLimiter(rate.Limit(float64(limit)/60.0), limit)
		entry = &userLimiter{limiter: l, limit: limit, lastUse: now}
		rlReg.m[userID] = entry
	}
	entry.lastUse = now
	return entry.limiter.Allow()
}

// dropUserLimiter removes a user's limiter (admin deleted the user or
// changed their rate limit).
func dropUserLimiter(userID int) {
	rlReg.mu.Lock()
	defer rlReg.mu.Unlock()
	delete(rlReg.m, userID)
}

// sweepIdleLimiters evicts entries unused for idleEvictAfter.
func sweepIdleLimiters() {
	cutoff := time.Now().Add(-idleEvictAfter)
	rlReg.mu.Lock()
	defer rlReg.mu.Unlock()
	for id, e := range rlReg.m {
		if e.lastUse.Before(cutoff) {
			delete(rlReg.m, id)
		}
	}
}
