package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The per-minute limit moved from an inline check in each handler to
// rateLimitMiddleware so that every /v1/ route is bounded, including
// /v1/models — the one endpoint a client could previously hammer for free.
// These tests pin the middleware's behaviour end to end through the real
// router: consumption, the 429 envelope per path, the missing Retry-After,
// and the OPTIONS skip.
//
// Each test seeds its own user with a rate limit of a distinct value: the
// in-memory limiter is process-global and buckets are keyed by user ID, which
// restarts at 1 in every fresh test database. A limit change recreates the
// bucket, so distinct limits isolate the tests without resetting the global.

// rlSeedUser creates an active user and returns nothing; the key is known to
// the caller.
func rlSeedUser(t *testing.T, name, key string, rateLimit int) {
	t.Helper()
	if err := createUser(name, key, rateLimit); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if _, err := db.Exec("UPDATE users SET status='active' WHERE name=?", name); err != nil {
		t.Fatalf("activate user: %v", err)
	}
}

// rlDrive runs one request through the full production router. key may be
// "" to omit the Authorization header.
func rlDrive(t *testing.T, method, target, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	setupRouter().ServeHTTP(rec, r)
	return rec
}

// A request inside the limit is admitted (it costs one token); the request
// beyond the limit is a 429 in the OpenAI envelope. The middleware must not
// send Retry-After: the bucket refills gradually, so any fixed value would be
// a guess.
func TestRateLimitChatCompletionsReturns429WithOpenAIEnvelope(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	rlSeedUser(t, "rl-chat", "sk-gw-rl-chat", 3)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for i := 1; i <= 3; i++ {
		rec := rlDrive(t, http.MethodPost, "/v1/chat/completions", "sk-gw-rl-chat", body)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d of 3 was rate limited; the burst of 3 must be admitted", i)
		}
	}

	rec := rlDrive(t, http.MethodPost, "/v1/chat/completions", "sk-gw-rl-chat", body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request = %d %s, want 429", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `"type":"rate_limit_error"`) {
		t.Errorf("429 body missing OpenAI rate_limit_error type: %s", got)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q; the token bucket refills gradually, so no value is true", ra)
	}
}

// The regression these tests exist for: /v1/models is rate limited now that
// the check lives in middleware. With a burst of two, the third /v1/models
// request must be a 429, not another free listing.
func TestRateLimitCoversModelsRoute(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	// One engine with a scripted model list, so the admitted requests answer
	// 200 from the probe cache without touching a network.
	resetProbeCache(t)
	eng := &fakeEngine{models: []string{"rl-model"}}
	engineCachePtr.Store(&engineCache{
		engines:  []EngineConfig{{ID: 1, Name: "rl-engine", Type: EngineOpenAICompat, BaseURL: "http://rl-fake"}},
		adapters: map[int]Engine{1: eng},
	})
	probeEngineCached(1, eng)

	rlSeedUser(t, "rl-models", "sk-gw-rl-models", 2)
	for i := 1; i <= 2; i++ {
		rec := rlDrive(t, http.MethodGet, "/v1/models", "sk-gw-rl-models", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("models request %d of 2 = %d %s, want 200", i, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "rl-model") {
			t.Fatalf("models response missing the listed model: %s", rec.Body.String())
		}
	}

	rec := rlDrive(t, http.MethodGet, "/v1/models", "sk-gw-rl-models", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd /v1/models request = %d, want 429 — the model list must not be free to hammer", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"type":"rate_limit_error"`) {
		t.Errorf("429 body missing rate_limit_error type: %s", got)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none", ra)
	}
}

// OPTIONS /v1/* is skipped by the middleware. The production router answers
// OPTIONS on a preflight route registered outside the chain, so the skip
// logic itself is driven here through the middleware directly: a request that
// reaches the handler after the bucket is exhausted proves the OPTIONS cost
// nothing.
func TestRateLimitSkipsOptions(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	rlSeedUser(t, "rl-options", "sk-gw-rl-options", 1)

	reached := 0
	h := apiAuthMiddleware(rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-gw-rl-options")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request = %d, want the handler (200)", rec.Code)
	}
	if reached != 1 {
		t.Fatalf("handler reached %d times, want 1", reached)
	}

	// Bucket is now empty. An OPTIONS must still reach the handler — if the
	// middleware charged it, this would be a 429.
	opts := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	opts.Header.Set("Authorization", "Bearer sk-gw-rl-options")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, opts)
	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS = %d, want 200 — OPTIONS must be skipped", rec.Code)
	}
	if reached != 2 {
		t.Fatalf("handler reached %d times after OPTIONS, want 2 — the OPTIONS consumed a token", reached)
	}

	// The bucket really is exhausted: the next authenticated request is a 429,
	// which is what makes the two 200s above meaningful.
	third := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	third.Header.Set("Authorization", "Bearer sk-gw-rl-options")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, third)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request after OPTIONS = %d, want 429", rec.Code)
	}

	// And on the wire the preflight still answers 204, never 429.
	if rec := rlDrive(t, http.MethodOptions, "/v1/models", "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS through the router = %d, want 204", rec.Code)
	}
}

// The Anthropic path rejects with Anthropic's envelope, not OpenAI's: Claude
// Code reads error.type from {"type":"error","error":{...}} to decide how to
// react, and the OpenAI shape would leave it guessing.
func TestRateLimitMessagesUsesAnthropicEnvelope(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	withMaxMsgLen(t, 1024)
	rlSeedUser(t, "rl-anthropic", "sk-gw-rl-anthropic", 4)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	for i := 1; i <= 4; i++ {
		if rec := rlDrive(t, http.MethodPost, "/v1/messages", "sk-gw-rl-anthropic", body); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d of 4 was rate limited; the burst of 4 must be admitted", i)
		}
	}

	rec := rlDrive(t, http.MethodPost, "/v1/messages", "sk-gw-rl-anthropic", body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("5th /v1/messages request = %d %s, want 429", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if !strings.HasPrefix(got, `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit exceeded"}`) {
		t.Errorf("429 body is not the Anthropic envelope: %s", got)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none", ra)
	}
}
