package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A recovered panic has to be reported in whatever format the client is already
// committed to parsing. These tests pin the three cases writePanicResponse
// distinguishes, because getting them wrong is invisible until a client breaks:
// a JSON body appended to an SSE stream still looks like a 200 to curl.

// panicOn returns a handler that panics after running setup, which lets each
// test put the response into a particular state first.
func panicOn(setup func(http.ResponseWriter)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if setup != nil {
			setup(w)
		}
		panic("boom")
	})
}

func TestPanicOnAPIPathReturnsJSONEnvelope(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	recoveryMiddleware(panicOn(nil)).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// The envelope must be the same shape as every other /v1 error, or a client
	// that reads err.error.message on a 429 crashes on a 500.
	var got ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not the OpenAI error envelope: %v (body=%q)", err, rr.Body.String())
	}
	if got.Error.Type != "api_error" {
		t.Errorf("error.type = %q, want api_error", got.Error.Type)
	}
	if got.Error.Message == "" {
		t.Error("error.message is empty")
	}
	if strings.Contains(got.Error.Message, "boom") {
		t.Errorf("panic value leaked to the client: %q", got.Error.Message)
	}
}

func TestPanicOnAdminPathStaysPlainText(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)

	recoveryMiddleware(panicOn(nil)).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		t.Errorf("admin path got a JSON envelope (Content-Type %q); "+
			"the OpenAI error shape is meaningless to the browser UI", ct)
	}
}

func TestPanicMidStreamEmitsSSEFrameNotJSONBody(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Reproduce the state streamCompletion leaves the response in: headers out,
	// 200 committed, one content frame already delivered.
	recoveryMiddleware(panicOn(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
	})).ServeHTTP(rr, req)

	// The status was committed before the panic and cannot be rewritten.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a committed status must not be rewritten", rr.Code)
	}

	body := rr.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream not terminated with [DONE]; body=%q", body)
	}

	// Every line after the headers must still be a well-formed SSE frame. A raw
	// JSON object here is the actual bug this guards: it parses as neither a
	// delta nor a terminator, and an accumulating client either stalls or errors.
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("non-SSE line written into a committed stream: %q", line)
		}
	}

	// The error frame itself must carry the envelope, so a client can tell a
	// truncated completion from a complete one.
	var sawError bool
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var env ErrorResponse
		if json.Unmarshal([]byte(payload), &env) == nil && env.Error.Type != "" {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("no error frame before [DONE]; client cannot tell the completion was truncated")
	}
}

func TestPanicAfterPartialBodyWritesNothingFurther(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)

	const partial = "<html><body>half a page"
	recoveryMiddleware(panicOn(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(partial))
	})).ServeHTTP(rr, req)

	// Appending an error to a half-rendered page produces a corrupt page and a
	// "superfluous WriteHeader" warning. Logging it is the only honest option.
	if got := rr.Body.String(); got != partial {
		t.Errorf("body was appended to after commit:\n got %q\nwant %q", got, partial)
	}
}

func TestRecoveredPanicDoesNotAffectNormalResponses(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	recoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, map[string]string{"object": "list"})
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := strings.TrimSpace(rr.Body.String()); body != `{"object":"list"}` {
		t.Errorf("body = %q, want the handler's own output unmodified", body)
	}
}

// The wrapper recoveryMiddleware adds must not hide http.Flusher, or
// streamCompletion's type assertion fails and streaming silently 500s.
func TestRecoveryWrapperPreservesFlusher(t *testing.T) {
	var sawFlusher bool
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	recoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawFlusher = w.(http.Flusher)
	})).ServeHTTP(rr, req)

	if !sawFlusher {
		t.Error("handler did not see an http.Flusher; SSE streaming would be refused")
	}
}
