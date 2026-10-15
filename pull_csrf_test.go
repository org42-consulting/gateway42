package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// A pull is a GET, which csrfMiddleware lets through, and the admin's
// SameSite=Lax cookie still rides along on a top-level cross-site navigation.
// The handler's own token check is the only thing between a hostile link and a
// multi-gigabyte download, so these tests pin it.

const testCSRF = "test-csrf-token"

func pullRequest(t *testing.T, query string, admin bool) *http.Request {
	t.Helper()
	return requestWithSession(t, http.MethodGet, "/admin/ollama-pull-stream?"+query,
		map[interface{}]interface{}{"admin": admin, csrfSessionKey: testCSRF})
}

func TestPullRejectsMissingCSRFToken(t *testing.T) {
	withSessionStore(t)
	rr := httptest.NewRecorder()
	handleOllamaPullStream(rr, pullRequest(t, "model=llama3", true))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a pull without a token", rr.Code)
	}
}

func TestPullRejectsWrongCSRFToken(t *testing.T) {
	withSessionStore(t)
	rr := httptest.NewRecorder()
	handleOllamaPullStream(rr, pullRequest(t, "model=llama3&_csrf=guess", true))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a wrong token", rr.Code)
	}
}

func TestPullRequiresAdminBeforeToken(t *testing.T) {
	withSessionStore(t)
	rr := httptest.NewRecorder()
	handleOllamaPullStream(rr, pullRequest(t, "model=llama3&_csrf="+testCSRF, false))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without an admin session", rr.Code)
	}
}

func TestPullWithValidTokenProceeds(t *testing.T) {
	withSessionStore(t)
	// No Ollama engine: the handler gets past both checks and reports that
	// over the event stream, which is what proves the token was accepted.
	withEngines(t, nil)

	rr := httptest.NewRecorder()
	handleOllamaPullStream(rr, pullRequest(t, "model=llama3&_csrf="+testCSRF, true))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rr.Body.String(), "No matching Ollama engine") {
		t.Errorf("body = %q, want the no-engine error frame", rr.Body.String())
	}
}

// The duplicate /admin/ollama-pull-search-stream route is gone; nothing may
// be left that reaches a pull without the token check.
func TestOnlyOnePullRoute(t *testing.T) {
	r := setupRouter()
	var match mux.RouteMatch
	if r.Match(httptest.NewRequest(http.MethodGet, "/admin/ollama-pull-search-stream", nil), &match) {
		t.Error("/admin/ollama-pull-search-stream still routes somewhere")
	}
	if !r.Match(httptest.NewRequest(http.MethodGet, "/admin/ollama-pull-stream", nil), &match) {
		t.Error("/admin/ollama-pull-stream no longer routes")
	}
}
