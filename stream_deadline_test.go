package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A stream used to be bounded by one wall-clock cap over its whole duration. On
// the local engine, which generated ~58 tokens/s, 120s is roughly 6.9k output
// tokens — so a long agent completion was cut mid-sentence and its client told
// "Stream interrupted" after already receiving thousands of good tokens. The
// bound is about silence now, not elapsed time: an engine that keeps sending
// bytes is alive however long it takes, and one that stops sending them is dead
// however little time has passed.

// withStreamBounds points the two stream bounds at test lengths and restores
// them afterwards. Not parallel-safe — they are process-wide.
func withStreamBounds(t *testing.T, idle, overall time.Duration) {
	t.Helper()
	prevIdle, prevMax := streamIdle, streamMax
	streamIdle, streamMax = idle, overall
	t.Cleanup(func() { streamIdle, streamMax = prevIdle, prevMax })
}

// quietChunkStream emits one chunk every step, then either ends (hang=false) or
// blocks until its context fires the way a read on a stalled engine does.
type quietChunkStream struct {
	ctx      context.Context
	step     time.Duration
	chunks   int
	hang     bool
	provided int
	closed   bool
	// stalled records that a read was cut short by the context rather than by
	// the stream ending on its own — proof that a capped stream releases the
	// upstream read instead of merely being abandoned.
	stalled bool
}

func (s *quietChunkStream) Recv() ([]byte, chunkMeta, error) {
	// Both cancellation returns report ctx.Err(), never the cause: that is
	// what a cancelled body read does, and it is why the handler must read the
	// deadline's cause itself rather than infer one from this error.
	if s.provided >= s.chunks {
		if !s.hang {
			return nil, chunkMeta{}, io.EOF
		}
		<-s.ctx.Done()
		s.stalled = true
		return nil, chunkMeta{}, s.ctx.Err()
	}
	// A read on a live connection dies the moment its context does, so waiting
	// for the next chunk has to watch for cancellation rather than sleep past
	// it. Without this the fake keeps producing after a cap, and the handler
	// has nothing to notice.
	if err := s.ctx.Err(); err != nil {
		s.stalled = true
		return nil, chunkMeta{}, err
	}
	select {
	case <-time.After(s.step):
	case <-s.ctx.Done():
		s.stalled = true
		return nil, chunkMeta{}, s.ctx.Err()
	}
	s.provided++
	text := strings.Repeat("x", s.provided)
	payload := `{"choices":[{"index":0,"delta":{"content":"` + text + `"}}]}`
	// Usage rides the terminal frame in both wire formats; carrying it here
	// keeps the loop's applyMeta on the path these tests exercise.
	meta := chunkMeta{auditText: text, outTokens: s.provided}
	if s.provided == s.chunks {
		meta.inTokens = 11
	}
	return []byte(payload), meta, nil
}

func (s *quietChunkStream) Close() error { s.closed = true; return nil }

// streamEngine hands StreamChat's context to its scripted stream, which is how a
// fake can block on cancellation the way a real upstream read does.
type streamEngine struct {
	stream ChatStream
}

func (e *streamEngine) Type() string          { return EngineOpenAICompat }
func (e *streamEngine) Name() string          { return "paced" }
func (e *streamEngine) baseURL() string       { return "http://paced" }
func (e *streamEngine) Status() (bool, error) { return true, nil }
func (e *streamEngine) ListModels() ([]map[string]interface{}, error) {
	return nil, nil
}
func (e *streamEngine) Chat(context.Context, map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}
func (e *streamEngine) StreamChat(ctx context.Context, _ map[string]interface{}) (ChatStream, error) {
	if s, ok := e.stream.(*quietChunkStream); ok {
		s.ctx = ctx
	}
	return e.stream, nil
}
func (e *streamEngine) TryAcquire(context.Context) bool { return true }
func (e *streamEngine) Release()                        {}

// ─────────────────────────── the deadline primitive ───────────────────────────

func TestStreamDeadlineIdleFiresOnSilence(t *testing.T) {
	withStreamBounds(t, 60*time.Millisecond, 0)
	d := newStreamDeadline(context.Background(), streamIdle, streamMax)
	defer d.stop()

	select {
	case <-d.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("idle deadline never fired")
	}
	if got := d.cause(); !errors.Is(got, errStreamIdle) {
		t.Errorf("cause = %v, want %v", got, errStreamIdle)
	}
}

// TestStreamDeadlineTouchesHoldItOpen is the regression that matters: steady
// progress past the idle bound must not end the stream. The loop runs 1.5x the
// idle gap, so a whole-duration cap of that length would have cut it.
func TestStreamDeadlineTouchesHoldItOpen(t *testing.T) {
	idle := 60 * time.Millisecond
	withStreamBounds(t, idle, 0)
	d := newStreamDeadline(context.Background(), streamIdle, streamMax)
	defer d.stop()

	started := time.Now()
	watch := started.Add(idle + idle/2)
	for time.Now().Before(watch) {
		select {
		case <-d.ctx.Done():
			t.Fatalf("capped after %v of steady progress, inside a %v idle gap",
				time.Since(started), idle)
		default:
		}
		time.Sleep(idle / 6)
		d.touch()
	}
	if cause := d.cause(); cause != nil {
		t.Errorf("cause = %v, want none while the stream keeps producing", cause)
	}
}

// TestStreamDeadlineOverallCeilingFiresDespiteProgress covers the backstop: an
// engine that dribbles bytes forever still gives up its concurrency slot.
func TestStreamDeadlineOverallCeilingFiresDespiteProgress(t *testing.T) {
	withStreamBounds(t, 10*time.Second, 80*time.Millisecond)
	d := newStreamDeadline(context.Background(), streamIdle, streamMax)
	defer d.stop()

	for {
		select {
		case <-d.ctx.Done():
			if got := d.cause(); !errors.Is(got, errStreamDeadline) {
				t.Errorf("cause = %v, want %v", got, errStreamDeadline)
			}
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
		d.touch()
	}
}

// A client hanging up is not a timeout. Mislabelling it as one would bury real
// disconnects under the timeout counter.
func TestStreamDeadlineClientDisconnectIsNotATimeout(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	d := newStreamDeadline(parent, time.Hour, time.Hour)
	defer d.stop()

	cancel()
	<-d.ctx.Done()
	if cause := d.cause(); cause != nil {
		t.Errorf("cause = %v, want none — a disconnect is not a timeout", cause)
	}
	if !errors.Is(d.ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() = %v, want context.Canceled", d.ctx.Err())
	}
}

func TestStreamDeadlineStopDisarmsTheTimer(t *testing.T) {
	withStreamBounds(t, 40*time.Millisecond, 0)
	d := newStreamDeadline(context.Background(), streamIdle, streamMax)
	d.stop()

	// The cause alone cannot show a leaked timer — stop() already cancelled, so
	// a later fire is a no-op. Ask the timer itself whether it is still armed:
	// a leaked one keeps the deadline (and its response body) reachable for the
	// whole idle window after the request is over.
	if d.timer.Stop() {
		t.Error("timer still armed after stop()")
	}
	time.Sleep(100 * time.Millisecond)
	if cause := d.cause(); cause != nil {
		t.Errorf("cause = %v after stop, want none — a finished request must not read as a timeout", cause)
	}
}

// TestStreamDeadlineCeilingPrecedesIdle checks the timer is armed for the sooner
// of the two bounds, so a ceiling shorter than the idle gap still fires.
func TestStreamDeadlineCeilingPrecedesIdle(t *testing.T) {
	withStreamBounds(t, time.Hour, 50*time.Millisecond)
	d := newStreamDeadline(context.Background(), streamIdle, streamMax)
	defer d.stop()

	if got := d.until(); got > 100*time.Millisecond {
		t.Errorf("armed for %v, want the 50ms ceiling rather than the 1h idle gap", got)
	}
	select {
	case <-d.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ceiling never fired")
	}
	if cause := d.cause(); !errors.Is(cause, errStreamDeadline) {
		t.Errorf("cause = %v, want %v", cause, errStreamDeadline)
	}
}

// TestDeadlineCauseSurvivesStop protects the ordering the audit depends on: the
// handler defers stop(), so reading the cause after the loop must still see the
// timeout that fired rather than the cancellation stop() records.
func TestDeadlineCauseSurvivesStop(t *testing.T) {
	withStreamBounds(t, 30*time.Millisecond, 0)
	d := newStreamDeadline(context.Background(), streamIdle, streamMax)
	<-d.ctx.Done()
	d.stop()

	if cause := d.cause(); !errors.Is(cause, errStreamIdle) {
		t.Errorf("cause = %v after stop, want the original %v", cause, errStreamIdle)
	}
}

// ───────────────────────────── through the handler ────────────────────────────

// runPacedStream drives streamCompletion with a stream that produces at a fixed
// pace and then either ends or falls silent.
func runPacedStream(t *testing.T, chunks int, step time.Duration, hang bool) (*httptest.ResponseRecorder, *requestDetail, *quietChunkStream) {
	t.Helper()
	withMaxMsgLen(t, 1000)
	drainInteractionLog()

	s := &quietChunkStream{chunks: chunks, step: step, hang: hang}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	d, r := newRequestDetail(r)
	w := httptest.NewRecorder()
	streamCompletion(w, r, &streamEngine{stream: s}, nil, 7, "paced-model",
		[]map[string]interface{}{{"role": "user", "content": "hi"}})
	return w, d, s
}

// TestActiveStreamOutlivesTheIdleGap is the chat42-mac-studio case: a completion
// that runs long while producing tokens reaches [DONE] instead of an error frame.
func TestActiveStreamOutlivesTheIdleGap(t *testing.T) {
	idle := 120 * time.Millisecond
	withStreamBounds(t, idle, 10*time.Second)

	start := time.Now()
	w, d, s := runPacedStream(t, 8, idle/3, false) // ~2.7x the idle gap, each read inside it
	elapsed := time.Since(start)

	body := w.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body ended without [DONE] after %v (idle gap %v): %s", elapsed, idle, body)
	}
	if strings.Contains(body, "Stream interrupted") {
		t.Errorf("a producing stream was interrupted after %v (idle gap %v): %s", elapsed, idle, body)
	}
	if elapsed < 2*idle {
		t.Errorf("ran %v, want well past the %v idle gap to prove the bound is per-gap", elapsed, idle)
	}
	if d.outcome != "" {
		t.Errorf("outcome = %q, want empty for a completion that finished", d.outcome)
	}
	if d.upstreamErr != "" {
		t.Errorf("upstream_error = %q, want empty", d.upstreamErr)
	}
	if d.inTokens != 11 || d.outTokens != 8 {
		t.Errorf("tokens = %d/%d, want 11/8 carried through the loop", d.inTokens, d.outTokens)
	}
	if !s.closed {
		t.Error("stream was not closed")
	}
}

// TestStalledStreamIsCappedAndAudited keeps the case the old cap got right, and
// closes the gap that made it undiagnosable: an engine that stops mid-stream is
// cut, and the row now says why.
func TestStalledStreamIsCappedAndAudited(t *testing.T) {
	idle := 60 * time.Millisecond
	withStreamBounds(t, idle, 10*time.Second)

	start := time.Now()
	w, d, s := runPacedStream(t, 2, time.Millisecond, true) // two frames, then silence
	elapsed := time.Since(start)

	body := w.Body.String()
	errAt := strings.Index(body, "Stream interrupted")
	if errAt < 0 {
		t.Errorf("body has no error frame for a stalled stream: %s", body)
	}
	// The terminator still follows the error frame — that is the existing SSE
	// contract, so a client stops waiting rather than hanging on a closed
	// stream. What matters is that it follows: a client that saw [DONE] first
	// would take the truncated answer as complete.
	if doneAt := strings.Index(body, "data: [DONE]"); errAt >= 0 && (doneAt < 0 || doneAt < errAt) {
		t.Errorf("error frame must precede the terminator: %s", body)
	}
	if d.outcome != "stream interrupted" {
		t.Errorf("outcome = %q, want \"stream interrupted\"", d.outcome)
	}
	if !strings.Contains(d.upstreamErr, errStreamIdle.Error()) {
		t.Errorf("upstream_error = %q, want it to name %q", d.upstreamErr, errStreamIdle)
	}
	if elapsed < idle || elapsed > 5*time.Second {
		t.Errorf("capped after %v, want just past the %v idle gap", elapsed, idle)
	}
	if !s.stalled {
		t.Error("the upstream read was never cancelled — the engine slot is still held")
	}
	// The two frames that did arrive must still be what the client was told.
	if got := auditedResponse(t, onlyInteractionRow(t)); !strings.Contains(got, "xx") {
		t.Errorf("interaction log lost the partial text: %q", got)
	}
}

// TestClientDisconnectIsNotRecordedAsTimeout keeps the two outcomes apart at the
// handler level: a failed write is a disconnect, whatever the deadline says.
func TestClientDisconnectIsNotRecordedAsTimeout(t *testing.T) {
	withStreamBounds(t, 10*time.Second, time.Minute)
	withMaxMsgLen(t, 1000)
	drainInteractionLog()

	s := &quietChunkStream{chunks: 5, step: time.Millisecond}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	d, r := newRequestDetail(r)
	w := &failingWriter{ResponseRecorder: httptest.NewRecorder()}
	streamCompletion(w, r, &streamEngine{stream: s}, nil, 7, "paced-model",
		[]map[string]interface{}{{"role": "user", "content": "hi"}})

	if d.outcome != "client disconnected" {
		t.Errorf("outcome = %q, want \"client disconnected\"", d.outcome)
	}
	if d.upstreamErr != "" {
		t.Errorf("upstream_error = %q, want empty for a client that hung up", d.upstreamErr)
	}
	if !s.closed {
		t.Error("stream was not closed after the client left")
	}
}

// onlyInteractionRow drains the interaction queue and requires exactly one row.
func onlyInteractionRow(t *testing.T) interactionLogEntry {
	t.Helper()
	rows := drainInteractionLog()
	if len(rows) != 1 {
		t.Fatalf("got %d interaction rows, want 1", len(rows))
	}
	return rows[0]
}
