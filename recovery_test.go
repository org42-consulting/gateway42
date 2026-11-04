package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
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

// The same wrapper must stay transparent about the writer's other capabilities.
// statusRecorder embeds the http.ResponseWriter interface, so the concrete
// server writer's Hijack and ReadFrom are not promoted through it: each has to
// be forwarded by hand or it quietly disappears, and the failure surfaces far
// from the cause — a refused upgrade, or static files copied through a buffer.

// Compile-time guards: the assertions handlers make are interface assertions,
// so a missing method compiles fine and fails at runtime, in production.
var (
	_ http.Hijacker = (*statusRecorder)(nil)
	_ io.ReaderFrom = (*statusRecorder)(nil)
)

// hijackRecorder stands in for the server's real ResponseWriter, which can
// hijack. httptest.ResponseRecorder cannot, so the test supplies it.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
	peers    []net.Conn
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	conn, peer := net.Pipe()
	h.peers = append(h.peers, peer)
	return conn, bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)), nil
}

func (h *hijackRecorder) closePeers() {
	for _, c := range h.peers {
		_ = c.Close()
	}
}

// readerFromRecorder reports whether the fast path was taken; httptest's
// recorder has no ReadFrom, so without one the delegation is unobservable.
type readerFromRecorder struct {
	*httptest.ResponseRecorder
	used bool
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.used = true
	return io.Copy(r.ResponseRecorder, src)
}

func TestStatusRecorderHijackDelegates(t *testing.T) {
	inner := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	t.Cleanup(inner.closePeers)
	rec := newStatusRecorder(inner)

	conn, rw, err := rec.Hijack()
	if err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	if conn == nil || rw == nil {
		t.Error("Hijack returned a nil connection or buffer")
	}
	if !inner.hijacked {
		t.Error("the underlying writer was never asked to hijack")
	}
	// A hijack commits the response: what follows are protocol bytes, not an
	// HTTP body, and the panic handler must not append to them.
	if !rec.wrote {
		t.Error("a successful hijack did not mark the response committed")
	}
}

func TestStatusRecorderHijackErrorLeavesResponseAmendable(t *testing.T) {
	// httptest's recorder cannot hijack, which is the case a handler would see
	// as a clean error rather than a panic.
	rec := newStatusRecorder(httptest.NewRecorder())
	if _, _, err := rec.Hijack(); err == nil {
		t.Error("want an error when the underlying writer cannot hijack")
	}
	if rec.wrote {
		t.Error("a failed hijack marked the response committed, so recovery would " +
			"refuse to write a status for a response that has no body yet")
	}
}

func TestPanicAfterHijackWritesNoBody(t *testing.T) {
	inner := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	t.Cleanup(inner.closePeers)
	rec := newStatusRecorder(inner)

	if _, _, err := rec.Hijack(); err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	writePanicResponse(rec, httptest.NewRequest(http.MethodGet, "/admin/panel", nil))

	if got := inner.Body.String(); got != "" {
		t.Errorf("panic recovery wrote %q onto a hijacked connection, which now "+
			"carries raw protocol bytes rather than an HTTP body", got)
	}
}

func TestStatusRecorderReadFromUsesFastPath(t *testing.T) {
	inner := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	rec := newStatusRecorder(inner)

	n, err := rec.ReadFrom(strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != int64(len("payload")) {
		t.Errorf("copied %d bytes, want %d", n, len("payload"))
	}
	if !inner.used {
		t.Error("io.Copy's fast path was not used; static files would be copied " +
			"through a buffer instead of handed to the kernel")
	}
	if inner.Body.String() != "payload" {
		t.Errorf("body = %q, want the payload", inner.Body.String())
	}
	if !rec.wrote {
		t.Error("ReadFrom did not mark the response committed")
	}
}

func TestStatusRecorderReadFromFallsBack(t *testing.T) {
	// httptest's recorder has no ReadFrom, so this exercises the fallback and
	// the commit flag on that path.
	inner := httptest.NewRecorder()
	rec := newStatusRecorder(inner)

	n, err := rec.ReadFrom(strings.NewReader("payload"))
	if err != nil || n != int64(len("payload")) {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if inner.Body.String() != "payload" {
		t.Errorf("body = %q, want the payload", inner.Body.String())
	}
	if !rec.wrote {
		t.Error("ReadFrom did not mark the response committed")
	}
}
