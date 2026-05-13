package main

import (
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
		defer metricInFlight.WithLabelValues(bucket).Dec()

		start := time.Now()
		rec := newStatusRecorder(w)
		next.ServeHTTP(rec, r)
		dur := time.Since(start).Seconds()

		metricRequestDuration.WithLabelValues(bucket, r.Method).Observe(dur)
		metricRequestsTotal.WithLabelValues(bucket, r.Method, strconv.Itoa(rec.status)).Inc()
	})
}

// observeUpstream times an upstream call and records duration + error count.
// op is "chat" | "stream" | "list_models" | "status".
func observeUpstream(engineType, op string, fn func() error) error {
	start := time.Now()
	err := fn()
	metricUpstreamDuration.WithLabelValues(engineType, op).Observe(time.Since(start).Seconds())
	if err != nil {
		metricUpstreamErrors.WithLabelValues(engineType, op).Inc()
	}
	return err
}

// handleMetrics serves /metrics, gated by an admin session.
func handleMetrics() http.Handler {
	h := promhttp.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAdminSession(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
