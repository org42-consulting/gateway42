package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// handleChatCompletions walks every engine that holds the requested model, in
// priority order, taking a concurrency slot from the first that has one. These
// tests pin the three outcomes of that walk: a saturated primary falls through
// to its failover target, a saturated fleet is a 503 with a Retry-After, and a
// model no engine holds is a 404.
//
// The route index is seeded directly (the way probe_cache_test.go does) so the
// request never blocks on a probe, and both engines report one slot each so a
// pre-acquire is exactly "saturated".

// foEngine is a fakeEngine with a test-chosen name and a record of Chat calls,
// so a test can tell which of two same-model engines the handler picked.
type foEngine struct {
	name  string
	calls []string
	fakeEngine
}

func (e *foEngine) Name() string { return e.name }

func (e *foEngine) Chat(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	e.calls = append(e.calls, "Chat")
	return nil, nil
}

// foSeedUser creates an active user for driving requests through the router.
func foSeedUser(t *testing.T, name, key string, rateLimit int) {
	t.Helper()
	if err := createUser(name, key, rateLimit); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if _, err := db.Exec("UPDATE users SET status='active' WHERE name=?", name); err != nil {
		t.Fatalf("activate user: %v", err)
	}
}

// foSeedPair seeds the engine cache with two fake engines that both report
// model and publishes a route index mapping model to both, primary first.
// Each engine's limiter has a single slot.
func foSeedPair(t *testing.T, model string) (*foEngine, *foEngine) {
	t.Helper()
	primary := &foEngine{name: "fo-primary", fakeEngine: fakeEngine{
		models: []string{model}, concurrencyLimiter: newConcurrencyLimiter(1),
	}}
	failover := &foEngine{name: "fo-failover", fakeEngine: fakeEngine{
		models: []string{model}, concurrencyLimiter: newConcurrencyLimiter(1),
	}}
	engineCachePtr.Store(&engineCache{
		engines: []EngineConfig{
			{ID: 1, Name: "fo-primary", Type: EngineOpenAICompat, BaseURL: "http://fo-1"},
			{ID: 2, Name: "fo-failover", Type: EngineOpenAICompat, BaseURL: "http://fo-2"},
		},
		adapters: map[int]Engine{1: primary, 2: failover},
	})
	gen := routeIndexGeneration()
	storeRouteIndex(&routeIndex{
		byModel: map[string][]int{model: {1, 2}},
		built:   time.Now(),
		probed:  true,
	}, gen)
	t.Cleanup(invalidateRouteIndex)
	return primary, failover
}

// foDrive posts one non-streaming completion for model through the full
// router as key.
func foDrive(t *testing.T, key, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	setupRouter().ServeHTTP(rec, r)
	return rec
}

// With the primary at its concurrency cap, the request must be served by the
// failover engine — not refused — and the audit row must say so.
func TestFailoverServesFromSecondEngineWhenPrimaryIsExhausted(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	foSeedUser(t, "fo-user", "sk-gw-fo", 60)

	primary, failover := foSeedPair(t, "fo-model")
	if !primary.TryAcquire(context.Background()) {
		t.Fatal("could not pre-acquire the primary's single slot")
	}

	drainRequestLog()
	rec := foDrive(t, "sk-gw-fo", "fo-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200 from the failover engine", rec.Code, rec.Body.String())
	}

	// The only slot that could have been taken was the failover's: the primary
	// never answered Chat…
	if len(primary.calls) != 0 {
		t.Errorf("primary Chat calls = %v, want none — the handler must not have reached it", primary.calls)
	}
	if len(failover.calls) != 1 {
		t.Fatalf("failover Chat calls = %v, want exactly one", failover.calls)
	}
	// …and the audit row carries the engine that served it.
	rows := drainRequestLog()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if rows[0].detail == nil || rows[0].detail.engineName != "fo-failover" {
		t.Errorf("audit engine = %+v, want fo-failover", rows[0].detail)
	}
	if rows[0].detail == nil || rows[0].detail.modelRequested != "fo-model" {
		t.Errorf("audit model = %+v, want fo-model", rows[0].detail)
	}
	// The handler releases the slot on every exit, so the failover is free
	// again after the response.
	if !failover.TryAcquire(context.Background()) {
		t.Error("failover slot still held after the response; the handler leaked it")
	}
	failover.Release()
}

// Every engine at its cap is a 503 with Retry-After: the client's retry logic
// keys on that header, and the wait budget (250ms per engine) makes "2" an
// honest lower bound.
func TestFailoverAllEnginesExhaustedReturns503(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	foSeedUser(t, "fo-busy", "sk-gw-fo-busy", 60)

	primary, failover := foSeedPair(t, "fo-model")
	if !primary.TryAcquire(context.Background()) || !failover.TryAcquire(context.Background()) {
		t.Fatal("could not pre-acquire both engines' slots")
	}

	rec := foDrive(t, "sk-gw-fo-busy", "fo-model")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %s, want 503 with both engines saturated", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra != "2" {
		t.Errorf("Retry-After = %q, want 2", ra)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"type":"api_error"`) {
		t.Errorf("503 body missing api_error type: %s", got)
	}
}

// A model that at least one probed engine does not hold is a 404, whatever
// the engines' saturation state.
func TestFailoverUnknownModelReturns404(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	foSeedUser(t, "fo-unknown", "sk-gw-fo-unknown", 60)

	foSeedPair(t, "fo-model")

	rec := foDrive(t, "sk-gw-fo-unknown", "ghost-model")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "not available on any configured engine") {
		t.Errorf("404 body missing the model-miss message: %s", got)
	}
	if !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Errorf("404 body missing invalid_request_error type: %s", rec.Body.String())
	}
}
