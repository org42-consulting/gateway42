package ratelimit

import (
	"testing"
	"time"
)

// The middleware tests in package main exercise the limiter through the full
// request path; these cover the bucket mechanics in isolation, which is the
// point of the package existing.

// A fresh bucket holds ratePerMinute tokens, so a burst of exactly that many
// is admitted and the next request is denied (the refill rate is
// ratePerMinute/60 per second — negligible over a test's milliseconds).
func TestTokenBucketAllowsBurstThenDenies(t *testing.T) {
	limiter := NewTokenBucketLimiter()

	for i := 0; i < 3; i++ {
		if !limiter.Allow(1, 3) {
			t.Fatalf("request %d of 3 denied, want the full burst admitted", i+1)
		}
	}
	if limiter.Allow(1, 3) {
		t.Fatal("4th request admitted, want 429 (bucket empty)")
	}
}

// A non-positive limit denies everything without creating a bucket: an admin
// disabling a user is a hard block, not a slow one.
func TestTokenBucketNonPositiveLimitDenies(t *testing.T) {
	limiter := NewTokenBucketLimiter()
	if limiter.Allow(1, 0) {
		t.Fatal("limit 0 admitted, want denied")
	}
	if limiter.Allow(1, -5) {
		t.Fatal("negative limit admitted, want denied")
	}
}

// Buckets are per-user: one user's exhaustion does not touch another's.
func TestTokenBucketIsPerUser(t *testing.T) {
	limiter := NewTokenBucketLimiter()
	for i := 0; i < 2; i++ {
		limiter.Allow(1, 2)
	}
	if limiter.Allow(1, 2) {
		t.Fatal("user 1 over budget admitted")
	}
	if !limiter.Allow(2, 2) {
		t.Fatal("user 2 admitted nothing, want their own full bucket")
	}
}

// Changing a user's limit rebuilds their bucket from a full burst — the admin
// raising a limit takes effect on the next request, not after a drain.
func TestTokenBucketLimitChangeRebuildsBucket(t *testing.T) {
	limiter := NewTokenBucketLimiter()
	for i := 0; i < 2; i++ {
		limiter.Allow(1, 2)
	}
	if limiter.Allow(1, 2) {
		t.Fatal("user 1 over their limit of 2 admitted")
	}
	if !limiter.Allow(1, 6) {
		t.Fatal("raised limit not admitted, want a rebuilt full bucket")
	}
}

// Forget clears the bucket, so the next request starts from a full burst —
// the user-deletion and limit-change path in the admin UI relies on this.
func TestTokenBucketForgetStartsFresh(t *testing.T) {
	limiter := NewTokenBucketLimiter()
	for i := 0; i < 2; i++ {
		limiter.Allow(1, 2)
	}
	if limiter.Allow(1, 2) {
		t.Fatal("user 1 over budget admitted")
	}
	limiter.Forget(1)
	if !limiter.Allow(1, 2) {
		t.Fatal("admission denied after Forget, want a fresh bucket")
	}
}

// ResetAll discards every bucket at once (the admin "Reset System" action).
func TestTokenBucketResetAll(t *testing.T) {
	limiter, ok := NewTokenBucketLimiter().(*TokenBucketLimiter)
	if !ok {
		t.Fatal("NewTokenBucketLimiter does not return a TokenBucketLimiter")
	}
	for i := 0; i < 2; i++ {
		limiter.Allow(1, 2)
		limiter.Allow(2, 2)
	}
	limiter.ResetAll()
	for _, userID := range []int{1, 2} {
		if !limiter.Allow(userID, 2) {
			t.Fatalf("user %d not admitted after ResetAll, want full burst", userID)
		}
	}
}

// SweepIdle evicts only entries unused for IdleEvictAfter; a just-touched
// bucket survives. The cutoff is driven through the package's exported
// constant so the test needs no clock faking.
func TestTokenBucketSweepIdleKeepsActiveEntries(t *testing.T) {
	limiter, ok := NewTokenBucketLimiter().(*TokenBucketLimiter)
	if !ok {
		t.Fatal("NewTokenBucketLimiter does not return a TokenBucketLimiter")
	}
	limiter.Allow(1, 2)

	// Force entry 2 to be idle: build it, then age it past the cutoff.
	limiter.Allow(2, 2)
	limiter.mu.Lock()
	limiter.m[2].lastUse = time.Now().Add(-2 * IdleEvictAfter)
	limiter.mu.Unlock()

	limiter.SweepIdle()

	limiter.mu.Lock()
	_, one := limiter.m[1]
	_, two := limiter.m[2]
	limiter.mu.Unlock()
	if !one {
		t.Fatal("actively used bucket was swept")
	}
	if two {
		t.Fatal("idle bucket survived the sweep")
	}
}
