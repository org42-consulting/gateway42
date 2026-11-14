package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// A model download is a GET (EventSource cannot POST), which csrfMiddleware
// lets through, and the admin's SameSite=Lax cookie still rides along on a
// top-level cross-site navigation. The handler's own token check is the only
// thing between a hostile link and a multi-gigabyte download onto the oMLX
// host, so these tests pin it.

const testCSRF = "test-csrf-token"

func downloadRequest(t *testing.T, query string, admin bool) *http.Request {
	t.Helper()
	return requestWithSession(t, http.MethodGet, "/admin/omlx-download-stream?"+query,
		map[interface{}]interface{}{"admin": admin, csrfSessionKey: testCSRF})
}

func TestDownloadRejectsMissingCSRFToken(t *testing.T) {
	withSessionStore(t)
	rr := httptest.NewRecorder()
	handleOMLXDownloadStream(rr, downloadRequest(t, "repo_id=mlx-community/Qwen3-8B-4bit", true))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a download without a token", rr.Code)
	}
}

func TestDownloadRejectsWrongCSRFToken(t *testing.T) {
	withSessionStore(t)
	rr := httptest.NewRecorder()
	handleOMLXDownloadStream(rr, downloadRequest(t, "repo_id=mlx-community/Qwen3-8B-4bit&_csrf=guess", true))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a wrong token", rr.Code)
	}
}

func TestDownloadRequiresAdminBeforeToken(t *testing.T) {
	withSessionStore(t)
	rr := httptest.NewRecorder()
	handleOMLXDownloadStream(rr, downloadRequest(t, "repo_id=mlx-community/Qwen3-8B-4bit&_csrf="+testCSRF, false))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without an admin session", rr.Code)
	}
}

func TestDownloadWithValidTokenProceeds(t *testing.T) {
	withSessionStore(t)
	// No oMLX engine: the handler gets past both checks and reports that
	// over the event stream, which is what proves the token was accepted.
	withEngines(t, nil)

	rr := httptest.NewRecorder()
	handleOMLXDownloadStream(rr, downloadRequest(t, "repo_id=mlx-community/Qwen3-8B-4bit&_csrf="+testCSRF, true))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rr.Body.String(), "No matching oMLX engine") {
		t.Errorf("body = %q, want the no-engine error frame", rr.Body.String())
	}
}

// The Ollama admin routes went with Ollama support; nothing may still reach
// them, and the oMLX ones replace them.
func TestOllamaAdminRoutesGone(t *testing.T) {
	r := setupRouter()
	var match mux.RouteMatch
	for _, p := range []string{"/admin/ollama-pull-stream", "/admin/ollama-pull-search-stream", "/admin/ollama-delete-model", "/admin/ollama-search"} {
		if r.Match(httptest.NewRequest(http.MethodGet, p, nil), &match) || r.Match(httptest.NewRequest(http.MethodPost, p, nil), &match) {
			t.Errorf("%s still routes somewhere", p)
		}
	}
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/admin/omlx-download-stream"}, {http.MethodGet, "/admin/omlx-search"}, {http.MethodPost, "/admin/omlx-model-action"},
	} {
		if !r.Match(httptest.NewRequest(rt.method, rt.path, nil), &match) {
			t.Errorf("%s %s does not route", rt.method, rt.path)
		}
	}
}
