package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func withMetricsToken(t *testing.T, token string) {
	t.Helper()
	prev := cfg.MetricsToken
	cfg.MetricsToken = token
	t.Cleanup(func() { cfg.MetricsToken = prev })
}

func scrape(t *testing.T, r *http.Request) int {
	t.Helper()
	rr := httptest.NewRecorder()
	handleMetrics().ServeHTTP(rr, r)
	return rr.Code
}

func TestMetricsAcceptsConfiguredBearerToken(t *testing.T) {
	withSessionStore(t)
	withMetricsToken(t, "scrape-secret")

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Header.Set("Authorization", "Bearer scrape-secret")
	if code := scrape(t, r); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the configured scrape token", code)
	}
}

func TestMetricsRejectsWrongBearerToken(t *testing.T) {
	withSessionStore(t)
	withMetricsToken(t, "scrape-secret")

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Header.Set("Authorization", "Bearer nope")
	if code := scrape(t, r); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a wrong token", code)
	}
}

// With no token configured the bearer path must be closed, not open to an
// empty "Bearer " header.
func TestMetricsBearerDisabledWhenUnset(t *testing.T) {
	withSessionStore(t)
	withMetricsToken(t, "")

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Header.Set("Authorization", "Bearer ")
	if code := scrape(t, r); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when GW42_METRICS_TOKEN is unset", code)
	}
}

func TestMetricsStillAcceptsAdminSession(t *testing.T) {
	withSessionStore(t)
	withMetricsToken(t, "scrape-secret")

	r := requestWithSession(t, http.MethodGet, "/metrics", map[interface{}]interface{}{"admin": true})
	if code := scrape(t, r); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an admin session", code)
	}
}
