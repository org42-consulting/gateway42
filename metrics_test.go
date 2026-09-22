package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A recovered panic has to stay visible in the two places an operator looks:
// the metrics and the audit log. Neither property shows up in normal operation,
// and both fail silently — a panic that is simply absent from
// gw42_http_requests_total looks exactly like a panic that never happened, and
// an audit row claiming 200 about a response that was a 500 is worse than no
// row at all. These tests pin both, and pin them against the real middleware
// order rather than a hand-rebuilt copy of it.

// outerTwo wraps h in the outermost two production middlewares, read out of the
// same slice setupRoutes applies. Swapping metrics and recovery back around
// therefore fails these tests instead of silently un-fixing the metrics. The
// rest of the chain is left off because apiAuthMiddleware would reject an
// unauthenticated /v1/ request long before the handler could panic.
func outerTwo(h http.Handler) http.Handler {
	chain := middlewareChain()
	return chain[0](chain[1](h))
}

// Reading deltas rather than absolute values keeps these tests independent of
// each other and of whatever else has touched the global registry.
func counted(path, method, status string) float64 {
	return testutil.ToFloat64(metricRequestsTotal.WithLabelValues(path, method, status))
}

func panicsCounted(path string) float64 {
	return testutil.ToFloat64(metricPanics.WithLabelValues(path))
}

// drainRequestLog empties the audit queue and returns what was in it. The
// flusher goroutine that would normally consume the channel is not running
// under test, so entries accumulate in its buffer and can be read back.
func drainRequestLog() []requestLogEntry {
	var out []requestLogEntry
	for {
		select {
		case e := <-chRequest:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestPanicBeforeCommitIsCountedAs500(t *testing.T) {
	const bucket = "/v1/chat/completions"
	before500 := counted(bucket, "POST", "500")
	beforePanics := panicsCounted(bucket)

	rr := httptest.NewRecorder()
	outerTwo(panicOn(nil)).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, bucket, nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}

	// This is the ordering assertion. If recovery is moved back outside metrics,
	// metrics observes before the 500 is written and this lands on status=200.
	if got := counted(bucket, "POST", "500") - before500; got != 1 {
		t.Errorf("gw42_http_requests_total{status=\"500\"} delta = %v, want 1; "+
			"a recovered panic must not be missing from the request count", got)
	}
	if got := panicsCounted(bucket) - beforePanics; got != 1 {
		t.Errorf("gw42_panics_total delta = %v, want 1", got)
	}
}

func TestMidStreamPanicIsCountedAsTheCommittedStatus(t *testing.T) {
	const bucket = "/v1/chat/completions"
	before200 := counted(bucket, "POST", "200")
	before500 := counted(bucket, "POST", "500")
	beforePanics := panicsCounted(bucket)

	rr := httptest.NewRecorder()
	outerTwo(panicOn(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, bucket, nil))

	// 200 is what the client received, so 200 is what the status metric has to
	// say. Recording 500 here would contradict both the wire and the audit log.
	if got := counted(bucket, "POST", "200") - before200; got != 1 {
		t.Errorf("committed status not counted: delta = %v, want 1", got)
	}
	if got := counted(bucket, "POST", "500") - before500; got != 0 {
		t.Errorf("mid-stream panic counted as 500 (delta %v); the 200 was already "+
			"on the wire and cannot be rewritten", got)
	}

	// Which is precisely why panics need their own counter: in
	// gw42_http_requests_total this request is indistinguishable from a healthy one.
	if got := panicsCounted(bucket) - beforePanics; got != 1 {
		t.Errorf("gw42_panics_total delta = %v, want 1; without this counter a "+
			"mid-stream panic is invisible to metrics", got)
	}
}

func TestInFlightGaugeUnwindsOnPanic(t *testing.T) {
	const bucket = "/v1/chat/completions"
	before := testutil.ToFloat64(metricInFlight.WithLabelValues(bucket))

	rr := httptest.NewRecorder()
	outerTwo(panicOn(nil)).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, bucket, nil))

	if got := testutil.ToFloat64(metricInFlight.WithLabelValues(bucket)); got != before {
		t.Errorf("gw42_inflight_requests = %v, want %v; a leaked in-flight count "+
			"drifts the gauge up permanently, one per panic", got, before)
	}
}

// A panic can also get past recoveryMiddleware — one raised above it, or in it.
// net/http drops that connection, so no status ever reaches the client and the
// recorder is left holding an untouched 200. Recording the 500 it would have
// been is the closer answer, and either way the request must not go unrecorded:
// this is the case metricsMiddleware's defer exists for, since the reorder alone
// does nothing here.
func TestPanicEscapingRecoveryIsStillCounted(t *testing.T) {
	const bucket = "/v1/models"
	before500 := counted(bucket, "GET", "500")
	before200 := counted(bucket, "GET", "200")
	beforeGauge := testutil.ToFloat64(metricInFlight.WithLabelValues(bucket))

	func() {
		defer func() { _ = recover() }() // stand in for net/http's own recovery
		metricsMiddleware(panicOn(nil)).
			ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, bucket, nil))
	}()

	if got := counted(bucket, "GET", "500") - before500; got != 1 {
		t.Errorf("gw42_http_requests_total{status=\"500\"} delta = %v, want 1; an "+
			"unrecovered panic must not be missing from the request count", got)
	}
	if got := counted(bucket, "GET", "200") - before200; got != 0 {
		t.Errorf("unrecovered panic counted as 200 (delta %v); no status reached "+
			"the client at all", got)
	}
	if got := testutil.ToFloat64(metricInFlight.WithLabelValues(bucket)); got != beforeGauge {
		t.Errorf("gw42_inflight_requests = %v, want %v", got, beforeGauge)
	}
}

func TestNormalRequestLeavesPanicCounterAlone(t *testing.T) {
	const bucket = "/v1/models"
	beforePanics := panicsCounted(bucket)
	before200 := counted(bucket, "GET", "200")

	rr := httptest.NewRecorder()
	outerTwo(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, map[string]string{"object": "list"})
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, bucket, nil))

	if got := panicsCounted(bucket) - beforePanics; got != 0 {
		t.Errorf("gw42_panics_total moved by %v on a healthy request", got)
	}
	if got := counted(bucket, "GET", "200") - before200; got != 1 {
		t.Errorf("gw42_http_requests_total{status=\"200\"} delta = %v, want 1", got)
	}
}

// Metrics, recovery and the request log all need the status. Each used to wrap
// the writer itself, stacking recorders per request; only the innermost saw the
// handler's own WriteHeader and the outer ones agreed with it by luck. Sharing
// one is what lets metrics read the status recovery wrote.
func TestChainInstallsExactlyOneStatusRecorder(t *testing.T) {
	var depth int
	rr := httptest.NewRecorder()

	outerTwo(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			rec, ok := w.(*statusRecorder)
			if !ok {
				return
			}
			depth++
			w = rec.ResponseWriter
		}
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if depth != 1 {
		t.Errorf("handler saw %d nested statusRecorders, want 1", depth)
	}
}

// The audit log sits inside recovery, so when its deferred write runs the 500
// has not been written yet. panicStatus is the shared answer to "what will the
// client see", which is what keeps the row and the response from disagreeing.
func TestPanickedRequestStillProducesAuditRow(t *testing.T) {
	drainRequestLog()

	const path = "/v1/chat/completions"
	rr := httptest.NewRecorder()
	recoveryMiddleware(requestLoggingMiddleware(panicOn(nil))).
		ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))

	rows := drainRequestLog()
	if len(rows) != 1 {
		t.Fatalf("got %d audit rows, want 1; a panicked request must still be "+
			"auditable", len(rows))
	}
	if rows[0].status != http.StatusInternalServerError {
		t.Errorf("audit row status = %d, want 500 — the row must match what the "+
			"client received, not the 200 the recorder still held", rows[0].status)
	}
	if rows[0].path != path {
		t.Errorf("audit row path = %q, want %q", rows[0].path, path)
	}
}

func TestMidStreamPanicAuditRowKeepsTheCommittedStatus(t *testing.T) {
	drainRequestLog()

	rr := httptest.NewRecorder()
	recoveryMiddleware(requestLoggingMiddleware(panicOn(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {}\n\n"))
	}))).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	rows := drainRequestLog()
	if len(rows) != 1 {
		t.Fatalf("got %d audit rows, want 1", len(rows))
	}
	if rows[0].status != http.StatusOK {
		t.Errorf("audit row status = %d, want 200; the status was committed before "+
			"the panic, so that is what the client saw", rows[0].status)
	}
}

func TestPanicStatusReflectsCommitment(t *testing.T) {
	fresh := newStatusRecorder(httptest.NewRecorder())
	if got := panicStatus(fresh); got != http.StatusInternalServerError {
		t.Errorf("uncommitted: panicStatus = %d, want 500", got)
	}

	committed := newStatusRecorder(httptest.NewRecorder())
	committed.WriteHeader(http.StatusOK)
	if got := panicStatus(committed); got != http.StatusOK {
		t.Errorf("committed: panicStatus = %d, want 200", got)
	}

	// An implicit 200 from a bare Write counts as committed too, which is the
	// case a rendered template hits.
	implicit := newStatusRecorder(httptest.NewRecorder())
	implicit.Write([]byte("half a page"))
	if got := panicStatus(implicit); got != http.StatusOK {
		t.Errorf("implicit commit: panicStatus = %d, want 200", got)
	}
}
