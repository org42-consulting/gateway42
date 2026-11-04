package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A wrong admin password costs 260,000 PBKDF2 rounds at the one endpoint that
// needs no credential, so the guard has to bound both the guessing and the CPU.
// The unit tests drive `now` directly rather than sleeping, so the lockout and
// the doubling are pinned exactly.

// resetLoginGuard isolates the package-level guard from whatever an earlier test
// left behind, and restores it afterwards.
func resetLoginGuard(t *testing.T) {
	t.Helper()
	prev := adminLogin
	adminLogin = &loginGuard{}
	t.Cleanup(func() { adminLogin = prev })
}

// burnFailures spends the whole allowance and returns the lockout the last
// failure triggered, advanced past so the next round starts unblocked.
func burnFailures(g *loginGuard, now time.Time) (time.Time, time.Duration) {
	var lockout time.Duration
	for i := 0; i < loginMaxFailures; i++ {
		if d := g.failed(now); d > 0 {
			lockout = d
		}
	}
	return now.Add(lockout + time.Second), lockout
}

func TestLoginGuardAllowsUntilTheAllowanceIsSpent(t *testing.T) {
	var g loginGuard
	now := time.Now()

	for i := 1; i < loginMaxFailures; i++ {
		if ok, wait := g.allow(now); !ok {
			t.Fatalf("attempt %d refused before the allowance was spent (wait %v)", i, wait)
		}
		if d := g.failed(now); d != 0 {
			t.Fatalf("attempt %d opened a lockout of %v early", i, d)
		}
	}
	if ok, _ := g.allow(now); !ok {
		t.Fatal("refused with one attempt still unspent")
	}

	if d := g.failed(now); d != loginBaseLockout {
		t.Fatalf("the final attempt opened a lockout of %v, want %v", d, loginBaseLockout)
	}
	if ok, wait := g.allow(now); ok || wait != loginBaseLockout {
		t.Errorf("allow during the lockout = (%v, %v), want refused for the full %v",
			ok, wait, loginBaseLockout)
	}
	// Self-healing: once the lockout passes, attempts are authenticated again.
	if ok, _ := g.allow(now.Add(loginBaseLockout)); !ok {
		t.Error("still refused after the lockout elapsed")
	}
}

func TestLoginGuardLockoutDoublesThenCaps(t *testing.T) {
	var g loginGuard
	now := time.Now()

	// 1m, 2m, 4m, 8m, then the 15m ceiling — each breach costs a further full
	// allowance to extend, so persistence is punished, not typos.
	want := []time.Duration{
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		loginMaxLockout,
		loginMaxLockout,
	}
	for i, w := range want {
		var got time.Duration
		now, got = burnFailures(&g, now)
		if got != w {
			t.Errorf("breach %d = %v, want %v", i+1, got, w)
		}
	}
}

func TestLoginGuardSuccessClearsFailuresAndLockout(t *testing.T) {
	var g loginGuard
	now := time.Now()

	// Near-misses that never spent the allowance must not shorten the next lockout.
	for i := 0; i < loginMaxFailures-1; i++ {
		g.failed(now)
	}
	g.succeeded()
	for i := 1; i < loginMaxFailures; i++ {
		if ok, _ := g.allow(now); !ok {
			t.Fatalf("locked out after %d failures following a success", i)
		}
		g.failed(now)
	}
	if d := g.failed(now); d != loginBaseLockout {
		t.Errorf("first breach after a success = %v, want the base %v, not a doubled one",
			d, loginBaseLockout)
	}

	// A success lifts a lockout already in force.
	g.succeeded()
	if ok, _ := g.allow(now); !ok {
		t.Error("success did not lift an active lockout")
	}
}

// postLogin submits the login form the way the browser does, straight to the
// handler; the CSRF middleware that fronts it in a real request is covered by
// the middleware tests.
func postLogin(t *testing.T, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"password": {password}}
	r := httptest.NewRequest(http.MethodPost, "/admin",
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handleAdminLogin(rec, r)
	return rec
}

func TestAdminLoginThrottlesAfterFailedAttempts(t *testing.T) {
	withTestDB(t)
	withSessionStore(t)
	resetLoginGuard(t)

	for i := 1; i <= loginMaxFailures; i++ {
		rec := postLogin(t, "wrong-password")
		if rec.Code != http.StatusFound {
			t.Fatalf("attempt %d = %d, want the usual 302 back to the login page", i, rec.Code)
		}
	}

	rec := postLogin(t, "wrong-password")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the allowance is spent", rec.Code)
	}
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs < 1 || secs > int(loginBaseLockout/time.Second) {
		t.Errorf("Retry-After = %q, want 1…%d seconds", rec.Header().Get("Retry-After"),
			int(loginBaseLockout/time.Second))
	}
	if !strings.Contains(rec.Body.String(), "Too many failed attempts") {
		t.Errorf("body = %q, want a message the operator can act on", rec.Body.String())
	}

	// The point of the guard: the expensive compare is not reached at all while
	// locked out, so a correct password is refused too until the lockout passes.
	if rec := postLogin(t, "admin123"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("correct password accepted during the lockout (status %d); "+
			"the guard must be consulted before verifying", rec.Code)
	}
}

func TestAdminLoginSuccessResetsTheGuard(t *testing.T) {
	withTestDB(t)
	withSessionStore(t)
	resetLoginGuard(t)

	for i := 0; i < loginMaxFailures-1; i++ {
		if rec := postLogin(t, "wrong-password"); rec.Code != http.StatusFound {
			t.Fatalf("failed attempt %d = %d, want 302", i+1, rec.Code)
		}
	}
	if rec := postLogin(t, "admin123"); rec.Code != http.StatusFound {
		t.Fatalf("correct password = %d, want 302 to the panel", rec.Code)
	}

	// A full allowance is available again rather than half of it.
	for i := 1; i < loginMaxFailures; i++ {
		if rec := postLogin(t, "wrong-password"); rec.Code != http.StatusFound {
			t.Fatalf("attempt %d after a success = %d, want 302", i, rec.Code)
		}
	}
	if ok, wait := adminLogin.allow(time.Now()); !ok {
		t.Errorf("locked out after only %d failures (wait %v)", loginMaxFailures-1, wait)
	}
}
