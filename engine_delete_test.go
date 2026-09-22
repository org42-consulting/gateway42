package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// OllamaDeleteModel used to discard resp.StatusCode entirely, so every refused
// delete returned nil and the admin UI reported success. These tests pin the
// status down, and pin the size of what an upstream can push into the error —
// that string reaches the browser through a flash stored in the session cookie.

// newDeleteServer stands up an Ollama-shaped /api/delete endpoint and returns
// the base URL and port split the way EngineConfig stores them.
func newDeleteServer(t *testing.T, status int, body string) (string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/delete" || r.Method != http.MethodDelete {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	host, port, ok := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if !ok {
		t.Fatalf("could not split %q", srv.URL)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	return "http://" + host, p
}

func TestOllamaDeleteModelSuccess(t *testing.T) {
	baseURL, port := newDeleteServer(t, http.StatusOK, `{}`)
	if err := OllamaDeleteModel(baseURL, port, "llama3"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestOllamaDeleteModelReportsRefusal(t *testing.T) {
	baseURL, port := newDeleteServer(t, http.StatusNotFound, `{"error":"model 'ghost' not found"}`)
	err := OllamaDeleteModel(baseURL, port, "ghost")
	if err == nil {
		t.Fatal("a 404 must not report success")
	}
	// The model name and the upstream reason both matter: the flash built from
	// this is the only thing the admin sees.
	for _, want := range []string{"ghost", "404", "not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestOllamaDeleteModelNonJSONBody(t *testing.T) {
	baseURL, port := newDeleteServer(t, http.StatusBadGateway, "<html>nginx\nbad gateway</html>")
	err := OllamaDeleteModel(baseURL, port, "llama3")
	if err == nil {
		t.Fatal("a 502 must not report success")
	}
	// Unparseable bodies are still shown, collapsed onto one line so the flash
	// markup does not break.
	if strings.ContainsAny(err.Error(), "\n\r") {
		t.Errorf("error spans multiple lines: %q", err)
	}
	if !strings.Contains(err.Error(), "bad gateway") {
		t.Errorf("error %q dropped the upstream body", err)
	}
}

func TestOllamaDeleteModelBoundsUpstreamBody(t *testing.T) {
	huge := strings.Repeat("A", 64*1024)
	baseURL, port := newDeleteServer(t, http.StatusInternalServerError, huge)
	err := OllamaDeleteModel(baseURL, port, "llama3")
	if err == nil {
		t.Fatal("a 500 must not report success")
	}
	// Session cookies cap out around 4KB; anything near that turns a readable
	// error into a failed save.
	if len(err.Error()) > 512 {
		t.Errorf("error is %d bytes, upstream body was not bounded", len(err.Error()))
	}
}

func TestUpstreamErrDetailEmptyBody(t *testing.T) {
	if got := upstreamErrDetail(strings.NewReader("")); got != "" {
		t.Errorf("empty body should add nothing, got %q", got)
	}
	if got := upstreamErrDetail(strings.NewReader("   \n\t ")); got != "" {
		t.Errorf("whitespace-only body should add nothing, got %q", got)
	}
}
