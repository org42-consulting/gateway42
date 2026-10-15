package main

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus collectors. All metric names use the gw42_ prefix.
var (
	metricRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gw42_http_requests_total",
		Help: "Total HTTP requests handled, partitioned by path bucket, method, and status.",
	}, []string{"path", "method", "status"})

	metricRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gw42_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.ExponentialBucketsRange(0.001, 60, 12),
	}, []string{"path", "method"})

	metricInFlight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gw42_inflight_requests",
		Help: "In-flight HTTP requests by path bucket.",
	}, []string{"path"})

	metricUpstreamDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gw42_upstream_duration_seconds",
		Help:    "Upstream engine call latency in seconds.",
		Buckets: prometheus.ExponentialBucketsRange(0.005, 120, 12),
	}, []string{"engine", "op"})

	metricUpstreamErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gw42_upstream_errors_total",
		Help: "Upstream engine call errors.",
	}, []string{"engine", "op"})

	metricLogsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gw42_logs_dropped_total",
		Help: "Log entries dropped due to writer-queue backpressure.",
	}, []string{"kind"}) // kind=interaction|request

	metricRateLimited = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gw42_rate_limited_total",
		Help: "Requests rejected by the in-memory rate limiter.",
	})

	metricUpstreamBusy = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gw42_upstream_busy_total",
		Help: "Requests rejected because the engine concurrency cap was full.",
	}, []string{"engine"})

	// Panics are counted apart from the status metric because the two answer
	// different questions. A panic before anything is written shows up as a 500
	// there; a panic mid-stream shows up as the 200 the client actually got,
	// since that status was committed before the panic and cannot be rewritten.
	// Only this counter sees both.
	metricPanics = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gw42_panics_total",
		Help: "Handler panics recovered by recoveryMiddleware, by path bucket.",
	}, []string{"path"})
)

// pathBucket collapses dynamic path segments so cardinality stays bounded.
// /v1/chat/completions and /v1/models stay distinct; everything else maps
// to its first-segment prefix (e.g. /admin/..., /toggle/..., /export/...).
func pathBucket(path string) string {
	switch path {
	case "/v1/chat/completions", "/v1/models", "/health", "/metrics", "/", "/admin", "/logout":
		return path
	}
	if strings.HasPrefix(path, "/admin/") {
		return "/admin/*"
	}
	if strings.HasPrefix(path, "/v1/") {
		return "/v1/*"
	}
	// numeric-tail routes like /toggle/42, /delete/42, /export/42, etc.
	if i := strings.Index(path[1:], "/"); i > 0 {
		return path[:i+1] + "/*"
	}
	return path
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket := pathBucket(r.URL.Path)
		metricInFlight.WithLabelValues(bucket).Inc()

		start := time.Now()
		rec := ensureRecorder(w)

		// What fixes the recovered-panic gap is the ordering, not this defer:
		// recoveryMiddleware runs inside this layer now, so it absorbs the panic
		// and writes its status before next.ServeHTTP returns here. Recording
		// sequentially would in fact work for that case.
		//
		// The defer is here so that this layer does not *depend* on a middleware
		// below it recovering panics. Sequential recording silently loses the
		// request the moment anything unwinds past this frame — which is what the
		// old outermost-recovery arrangement did on every single panic. Measuring
		// in a defer is the property that made that bug possible to have.
		//
		// completed distinguishes the two: false means the panic got past
		// recoveryMiddleware as well, the connection is about to be dropped by
		// net/http, and the recorder still holds an untouched 200 that no client
		// ever saw. panicStatus is the same answer the response and the audit log
		// use.
		completed := false
		defer func() {
			metricInFlight.WithLabelValues(bucket).Dec()

			status := rec.status
			if !completed {
				status = panicStatus(rec)
			}
			metricRequestDuration.WithLabelValues(bucket, r.Method).Observe(time.Since(start).Seconds())
			metricRequestsTotal.WithLabelValues(bucket, r.Method, strconv.Itoa(status)).Inc()
		}()

		next.ServeHTTP(rec, r)
		completed = true
	})
}

// observeUpstream times an upstream call and records duration + error count.
// op is "chat" | "stream" | "list_models" | "status".
func observeUpstream(engineType, op string, fn func() error) error {
	start := time.Now()
	err := fn()
	recordUpstream(engineType, op, start, err)
	return err
}

// recordUpstream is observeUpstream's recording half, for a call that cannot
// be wrapped in a closure — one that hands a live response body back to its
// caller, which bodyclose cannot follow through a captured variable.
func recordUpstream(engineType, op string, start time.Time, err error) {
	metricUpstreamDuration.WithLabelValues(engineType, op).Observe(time.Since(start).Seconds())
	if err != nil {
		metricUpstreamErrors.WithLabelValues(engineType, op).Inc()
	}
}

// handleMetrics serves /metrics to an admin session or, when
// GW42_METRICS_TOKEN is set, to a scraper presenting it as a Bearer token.
func handleMetrics() http.Handler {
	h := promhttp.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !metricsTokenValid(r) && !isAdminSession(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gw42-metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// metricsTokenValid reports whether r carries the configured scrape token.
//
// This is a separate credential from user API keys on purpose: a user key
// identifies a client of the /v1 API and is rate-limited per user, while this
// grants read access to operational data across all users and should be
// revocable without touching any of them. An unset token disables the path
// entirely rather than matching an empty header.
func metricsTokenValid(r *http.Request) bool {
	if cfg.MetricsToken == "" {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(cfg.MetricsToken)) == 1
}
