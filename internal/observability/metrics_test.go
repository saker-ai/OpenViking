package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsRecord(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsFor(reg)

	m.RecordHTTPRequest("GET", "/api/v1/foo", "200", 100*time.Millisecond)
	m.RecordHTTPRequest("POST", "/api/v1/bar", "201", 50*time.Millisecond)
	m.IncRagfsOps("read", "local", "ok")
	m.IncVectordbQuery("docs", "qdrant", "ok")
	m.IncQueueTasks("embed", "default", "done")
	m.ObserveIngestPipeline("web", "ok", 250*time.Millisecond)
	m.IncLLMCalls("openai", "gpt-4", "ok")

	expected := `
# HELP openviking_http_requests_total Total HTTP requests by method, route, and status.
# TYPE openviking_http_requests_total counter
openviking_http_requests_total{method="GET",path="/api/v1/foo",status="200"} 1
openviking_http_requests_total{method="POST",path="/api/v1/bar",status="201"} 1
# HELP openviking_llm_calls_total LLM API calls by provider, model, and status.
# TYPE openviking_llm_calls_total counter
openviking_llm_calls_total{model="gpt-4",provider="openai",status="ok"} 1
# HELP openviking_queue_tasks_total queuefs task outcomes by type, priority, and status.
# TYPE openviking_queue_tasks_total counter
openviking_queue_tasks_total{priority="default",status="done",type="embed"} 1
# HELP openviking_ragfs_ops_total ragfs operations by op, backend, and status.
# TYPE openviking_ragfs_ops_total counter
openviking_ragfs_ops_total{backend="local",op="read",status="ok"} 1
# HELP openviking_vectordb_query_total VectorDB queries by collection, backend, and status.
# TYPE openviking_vectordb_query_total counter
openviking_vectordb_query_total{backend="qdrant",collection="docs",status="ok"} 1
`
	err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"openviking_http_requests_total",
		"openviking_llm_calls_total",
		"openviking_queue_tasks_total",
		"openviking_ragfs_ops_total",
		"openviking_vectordb_query_total",
	)
	require.NoError(t, err)
}

func TestMetricsMiddlewareRecords(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsFor(reg)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(MetricsMiddleware(m))
	r.GET("/foo", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	expected := `
# HELP openviking_http_requests_total Total HTTP requests by method, route, and status.
# TYPE openviking_http_requests_total counter
openviking_http_requests_total{method="GET",path="/foo",status="200"} 1
`
	err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "openviking_http_requests_total")
	require.NoError(t, err)

	// In-flight gauge must be 0 after the request completes.
	assert.Equal(t, 0.0, testutil.ToFloat64(m.httpInFlight.WithLabelValues("GET", "/foo")))
}

func TestMetricsMiddlewareUnmatchedRoute(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsFor(reg)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(MetricsMiddleware(m))
	r.NoRoute(func(c *gin.Context) { c.Status(http.StatusNotFound) })

	req := httptest.NewRequest(http.MethodGet, "/no-such-path", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Unmatched routes label path as "unknown" to keep cardinality bounded.
	expected := `
# HELP openviking_http_requests_total Total HTTP requests by method, route, and status.
# TYPE openviking_http_requests_total counter
openviking_http_requests_total{method="GET",path="unknown",status="404"} 1
`
	err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "openviking_http_requests_total")
	require.NoError(t, err)
}
