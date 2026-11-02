package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The interaction log promises "prompt and response for each completion".
// Streamed completions used to be logged with the literal response
// "streamed", so the audit trail had no record of what any streaming client
// was told — and a client that hung up mid-stream was not logged at all.

// auditedResponse renders a logged row's response the way the writer stores
// it. Entries now carry raw values — encoding moved to the flush — and the
// stream path's response is assembled text, so it must already be a string.
func auditedResponse(t *testing.T, e interactionLogEntry) string {
	t.Helper()
	s, ok := e.response.(string)
	if !ok {
		t.Fatalf("stream path must log a string response, got %T", e.response)
	}
	return s
}

func drainInteractionLog() []interactionLogEntry {
	var out []interactionLogEntry
	for {
		select {
		case e := <-chInteraction:
			out = append(out, e)
		default:
			return out
		}
	}
}

func runStream(t *testing.T, w http.ResponseWriter, ctx context.Context, body string) []interactionLogEntry {
	t.Helper()
	withMaxMsgLen(t, 1000)
	drainInteractionLog()
	eng := &fakeEngine{stream: newPassthroughTestStream(body)}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	streamCompletion(w, r, eng, nil, 42, "m", []map[string]interface{}{{"role": "user", "content": "hi"}})
	return drainInteractionLog()
}

const helloStream = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}

data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`

func TestStreamedCompletionAuditsResponseText(t *testing.T) {
	rows := runStream(t, httptest.NewRecorder(), context.Background(), helloStream)
	if len(rows) != 1 {
		t.Fatalf("got %d interaction rows, want 1", len(rows))
	}
	if got := auditedResponse(t, rows[0]); got != "Hello" {
		t.Errorf("audited response = %q, want the streamed text \"Hello\"", got)
	}
	if rows[0].userID != 42 || rows[0].model != "m" {
		t.Errorf("row = %+v, want user 42 / model m", rows[0])
	}
}

func TestStreamedToolCallsAreAudited(t *testing.T) {
	rows := runStream(t, httptest.NewRecorder(), context.Background(),
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]}}]}

data: [DONE]
`)
	if len(rows) != 1 {
		t.Fatalf("got %d interaction rows, want 1", len(rows))
	}
	if got := auditedResponse(t, rows[0]); !strings.Contains(got, "get_weather") {
		t.Errorf("audited response = %q, want the tool call recorded", got)
	}
}

// failingWriter accepts the headers and the first frame, then reports the
// client gone, the way a write to a closed connection does.
type failingWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (f *failingWriter) Write(b []byte) (int, error) {
	f.writes++
	if f.writes > 1 {
		return 0, http.ErrHandlerTimeout
	}
	return f.ResponseRecorder.Write(b)
}

func TestStreamAbandonedByClientIsStillAudited(t *testing.T) {
	rows := runStream(t, &failingWriter{ResponseRecorder: httptest.NewRecorder()},
		context.Background(), helloStream)
	if len(rows) != 1 {
		t.Fatalf("got %d interaction rows, want 1 — a disconnected stream must still be audited", len(rows))
	}
	if got := auditedResponse(t, rows[0]); !strings.HasPrefix(got, "Hel") || !strings.Contains(got, "disconnected") {
		t.Errorf("audited response = %q, want the partial text marked as disconnected", got)
	}
}

func TestStreamAuditIsBounded(t *testing.T) {
	var a streamAudit
	a.limit = 10
	for i := 0; i < 100; i++ {
		// write takes the extracted text, not the frame: extraction is the
		// adapter's job now (see ChatStream.Recv).
		a.write("0123456789")
	}
	if n := len(a.String()); n > 10 {
		t.Errorf("audit buffer grew to %d bytes, want at most the limit of 10", n)
	}
}
