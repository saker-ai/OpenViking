// Package observability wires OpenTelemetry tracing, Prometheus metrics,
// and structured logging. The tracer provider returned by InitTracer is
// shutdown-aware so callers can flush spans on graceful exit.
package observability

import (
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

const metricNamespace = "openviking"

// defaultMetrics and defaultOnce guard singleton registration on the
// default Prometheus registry so repeat callers (e.g. BuildApp invoked
// by multiple tests) do not panic on duplicate registration.
var (
	defaultMetrics *Metrics
	defaultOnce    sync.Once
)

// Metrics holds every Prometheus metric vector exposed by the OpenViking
// server. Use NewMetrics to register them with the default registry at
// startup; use NewMetricsFor in tests to obtain an isolated registry.
type Metrics struct {
	httpRequestsTotal   *prometheus.CounterVec
	httpRequestDuration *prometheus.HistogramVec
	httpInFlight        *prometheus.GaugeVec
	ragfsOpsTotal       *prometheus.CounterVec
	vectordbQueryTotal  *prometheus.CounterVec
	queueTasksTotal     *prometheus.CounterVec
	ingestPipelineDur   *prometheus.HistogramVec
	llmCallsTotal       *prometheus.CounterVec
	// P2 additions: bring the metric surface from 8 to 20 vectors so the
	// stats collector and on-call runbooks have the signals they need.
	embedderLatency          *prometheus.HistogramVec
	rerankerLatency          *prometheus.HistogramVec
	vlmLatency               *prometheus.HistogramVec
	encryptionOpsTotal       *prometheus.CounterVec
	retrievalScore           *prometheus.HistogramVec
	memoryHotness            *prometheus.HistogramVec
	circuitBreakerState      *prometheus.GaugeVec
	circuitBreakerTransitions *prometheus.CounterVec
	tempUploadsInFlight      *prometheus.GaugeVec
	tempUploadBytes          *prometheus.CounterVec
	sessionActive            *prometheus.GaugeVec
	oauthTokenRefreshTotal   *prometheus.CounterVec
}

// NewMetrics creates and registers all metric vectors on the default
// Prometheus registry. Safe to call multiple times: the first call
// registers; subsequent calls return the same singleton. Tests that
// need isolation should use NewMetricsFor with a fresh registry.
func NewMetrics() *Metrics {
	defaultOnce.Do(func() {
		defaultMetrics = NewMetricsFor(prometheus.DefaultRegisterer)
	})
	return defaultMetrics
}

// NewMetricsFor creates and registers metric vectors on reg. Pass an
// isolated registry in tests to avoid colliding with the global state.
// A nil reg returns a configured but unregistered *Metrics.
func NewMetricsFor(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		httpRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "http_requests_total",
			Help:      "Total HTTP requests by method, route, and status.",
		}, []string{"method", "path", "status"}),
		httpRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request latency in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "path", "status"}),
		httpInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      "http_in_flight",
			Help:      "In-flight HTTP requests.",
		}, []string{"method", "path"}),
		ragfsOpsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "ragfs_ops_total",
			Help:      "ragfs operations by op, backend, and status.",
		}, []string{"op", "backend", "status"}),
		vectordbQueryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "vectordb_query_total",
			Help:      "VectorDB queries by collection, backend, and status.",
		}, []string{"collection", "backend", "status"}),
		queueTasksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "queue_tasks_total",
			Help:      "queuefs task outcomes by type, priority, and status.",
		}, []string{"type", "priority", "status"}),
		ingestPipelineDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "ingest_pipeline_duration_seconds",
			Help:      "Ingest pipeline stage duration in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"source", "status"}),
		llmCallsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "llm_calls_total",
			Help:      "LLM API calls by provider, model, and status.",
		}, []string{"provider", "model", "status"}),
		embedderLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "embedder_latency_seconds",
			Help:      "Embedder call latency in seconds by provider and status.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"provider", "status"}),
		rerankerLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "reranker_latency_seconds",
			Help:      "Reranker call latency in seconds by provider and status.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"provider", "status"}),
		vlmLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "vlm_latency_seconds",
			Help:      "VLM chat/completion call latency in seconds by provider and status.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"provider", "status"}),
		encryptionOpsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "encryption_ops_total",
			Help:      "Crypto envelope operations by op, provider, and status.",
		}, []string{"op", "provider", "status"}),
		retrievalScore: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "retrieval_score",
			Help:      "Final retrieval score distribution by stage.",
			Buckets:   []float64{0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99},
		}, []string{"stage"}),
		memoryHotness: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "memory_hotness",
			Help:      "Memory lifecycle hotness score distribution by account.",
			Buckets:   []float64{0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99},
		}, []string{"account"}),
		circuitBreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      "circuit_breaker_state",
			Help:      "Circuit breaker state by name (0=closed, 1=half-open, 2=open).",
		}, []string{"name"}),
		circuitBreakerTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "circuit_breaker_transitions_total",
			Help:      "Circuit breaker state transitions by name and direction.",
		}, []string{"name", "from", "to"}),
		tempUploadsInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      "temp_uploads_in_flight",
			Help:      "Active temp uploads by mode (local/shared).",
		}, []string{"mode"}),
		tempUploadBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "temp_upload_bytes_total",
			Help:      "Total bytes received for temp uploads by mode.",
		}, []string{"mode"}),
		sessionActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      "session_active",
			Help:      "Active sessions by account.",
		}, []string{"account"}),
		oauthTokenRefreshTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "oauth_token_refresh_total",
			Help:      "OAuth token refresh attempts by provider and status.",
		}, []string{"provider", "status"}),
	}
	if reg != nil {
		reg.MustRegister(
			m.httpRequestsTotal,
			m.httpRequestDuration,
			m.httpInFlight,
			m.ragfsOpsTotal,
			m.vectordbQueryTotal,
			m.queueTasksTotal,
			m.ingestPipelineDur,
			m.llmCallsTotal,
			m.embedderLatency,
			m.rerankerLatency,
			m.vlmLatency,
			m.encryptionOpsTotal,
			m.retrievalScore,
			m.memoryHotness,
			m.circuitBreakerState,
			m.circuitBreakerTransitions,
			m.tempUploadsInFlight,
			m.tempUploadBytes,
			m.sessionActive,
			m.oauthTokenRefreshTotal,
		)
	}
	return m
}

// RecordHTTPRequest increments the request counter and observes duration
// for the given method, route, and status. duration is rounded to seconds.
func (m *Metrics) RecordHTTPRequest(method, path, status string, duration time.Duration) {
	m.httpRequestsTotal.WithLabelValues(method, path, status).Inc()
	m.httpRequestDuration.WithLabelValues(method, path, status).Observe(duration.Seconds())
}

// IncRagfsOps increments the ragfs operations counter.
func (m *Metrics) IncRagfsOps(op, backend, status string) {
	m.ragfsOpsTotal.WithLabelValues(op, backend, status).Inc()
}

// IncVectordbQuery increments the vectordb query counter.
func (m *Metrics) IncVectordbQuery(collection, backend, status string) {
	m.vectordbQueryTotal.WithLabelValues(collection, backend, status).Inc()
}

// IncQueueTasks increments the queue tasks counter.
func (m *Metrics) IncQueueTasks(taskType, priority, status string) {
	m.queueTasksTotal.WithLabelValues(taskType, priority, status).Inc()
}

// ObserveIngestPipeline records ingest pipeline stage duration.
func (m *Metrics) ObserveIngestPipeline(source, status string, duration time.Duration) {
	m.ingestPipelineDur.WithLabelValues(source, status).Observe(duration.Seconds())
}

// IncLLMCalls increments the LLM call counter.
func (m *Metrics) IncLLMCalls(provider, model, status string) {
	m.llmCallsTotal.WithLabelValues(provider, model, status).Inc()
}

// ObserveEmbedderLatency records an embedder call's latency.
func (m *Metrics) ObserveEmbedderLatency(provider, status string, duration time.Duration) {
	m.embedderLatency.WithLabelValues(provider, status).Observe(duration.Seconds())
}

// ObserveRerankerLatency records a reranker call's latency.
func (m *Metrics) ObserveRerankerLatency(provider, status string, duration time.Duration) {
	m.rerankerLatency.WithLabelValues(provider, status).Observe(duration.Seconds())
}

// ObserveVLMLatency records a VLM call's latency.
func (m *Metrics) ObserveVLMLatency(provider, status string, duration time.Duration) {
	m.vlmLatency.WithLabelValues(provider, status).Observe(duration.Seconds())
}

// IncEncryptionOps increments the crypto envelope operations counter.
// op is "encrypt"/"decrypt"/"wrap"/"unwrap"; provider is "local"/"vault"/"volcengine".
func (m *Metrics) IncEncryptionOps(op, provider, status string) {
	m.encryptionOpsTotal.WithLabelValues(op, provider, status).Inc()
}

// ObserveRetrievalScore records a retrieval final score.
// stage is "rerank"/"final"/"hotness_blended".
func (m *Metrics) ObserveRetrievalScore(stage string, score float64) {
	m.retrievalScore.WithLabelValues(stage).Observe(score)
}

// ObserveMemoryHotness records a hotness score for an account.
func (m *Metrics) ObserveMemoryHotness(account string, score float64) {
	m.memoryHotness.WithLabelValues(account).Observe(score)
}

// SetCircuitBreakerState sets the gauge for a named breaker.
// state must be 0 (closed), 1 (half-open), or 2 (open).
func (m *Metrics) SetCircuitBreakerState(name string, state float64) {
	m.circuitBreakerState.WithLabelValues(name).Set(state)
}

// IncCircuitBreakerTransition records a state transition.
// from/to are "closed"/"half-open"/"open".
func (m *Metrics) IncCircuitBreakerTransition(name, from, to string) {
	m.circuitBreakerTransitions.WithLabelValues(name, from, to).Inc()
}

// IncTempUploadsInFlight increments the in-flight gauge; pair with
// DecTempUploadsInFlight in a defer.
func (m *Metrics) IncTempUploadsInFlight(mode string) {
	m.tempUploadsInFlight.WithLabelValues(mode).Inc()
}

// DecTempUploadsInFlight decrements the in-flight gauge.
func (m *Metrics) DecTempUploadsInFlight(mode string) {
	m.tempUploadsInFlight.WithLabelValues(mode).Dec()
}

// AddTempUploadBytes adds n bytes to the temp upload counter for mode.
func (m *Metrics) AddTempUploadBytes(mode string, n int64) {
	m.tempUploadBytes.WithLabelValues(mode).Add(float64(n))
}

// SetSessionActive sets the active-session gauge for an account.
func (m *Metrics) SetSessionActive(account string, n float64) {
	m.sessionActive.WithLabelValues(account).Set(n)
}

// IncOAuthTokenRefresh increments the OAuth refresh counter.
// provider is "feishu"/"google"/...; status is "ok"/"error"/"permanent_error".
func (m *Metrics) IncOAuthTokenRefresh(provider, status string) {
	m.oauthTokenRefreshTotal.WithLabelValues(provider, status).Inc()
}

// MetricsMiddleware returns a gin middleware that records RED metrics
// (request count, duration histogram, in-flight gauge) for every request.
// The path label uses the registered route template (c.FullPath) to keep
// cardinality bounded; unmatched routes label as "unknown".
func MetricsMiddleware(m *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if path == "" {
			path = "unknown"
		}
		method := c.Request.Method
		m.httpInFlight.WithLabelValues(method, path).Inc()
		defer m.httpInFlight.WithLabelValues(method, path).Dec()
		start := time.Now()
		c.Next()
		status := strconv.Itoa(c.Writer.Status())
		m.httpRequestsTotal.WithLabelValues(method, path, status).Inc()
		m.httpRequestDuration.WithLabelValues(method, path, status).Observe(time.Since(start).Seconds())
	}
}
