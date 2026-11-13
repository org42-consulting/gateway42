package main

import "gateway42/internal/ratelimit"

// Process-wide rate limiting. The limiter itself lives in
// internal/ratelimit (no Gateway42 globals); this file only wires it into the
// app: one default instance plus the admin actions that need to touch its
// in-memory state directly.

// defaultLimiter is the process-wide instance. Middleware and handlers use
// this; tests can swap it for a fake.
var defaultLimiter ratelimit.RateLimiter = ratelimit.NewTokenBucketLimiter()

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
	if tb, ok := defaultLimiter.(*ratelimit.TokenBucketLimiter); ok {
		tb.ResetAll()
	}
}

// sweepIdleLimiters evicts entries unused for IdleEvictAfter. It runs on the
// background ticker in main.go; like resetAllLimiters it is an in-memory
// detail exposed through a type assertion.
func sweepIdleLimiters() {
	if tb, ok := defaultLimiter.(*ratelimit.TokenBucketLimiter); ok {
		tb.SweepIdle()
	}
}
