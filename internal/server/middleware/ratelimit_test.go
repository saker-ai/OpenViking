package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRateLimitBurstThenReject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RateLimit(1, 2)) // 1 rps, burst 2
	hits := atomic.Int32{}
	r.GET("/", func(c *gin.Context) {
		hits.Add(1)
		c.Status(http.StatusOK)
	})

	// First two should pass (burst).
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/", nil))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusOK, rec1.Code)
	assert.Equal(t, http.StatusOK, rec2.Code)

	// Third should be rejected with 429 and Retry-After.
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusTooManyRequests, rec3.Code)
	assert.NotEmpty(t, rec3.Header().Get("Retry-After"))
	assert.Contains(t, rec3.Body.String(), "RATE_LIMITED")
	assert.Equal(t, int32(2), hits.Load())
}

func TestRateLimitPerAccountIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RateLimit(1, 1))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Account A uses its token.
	recA1 := httptest.NewRecorder()
	reqA1 := httptest.NewRequest(http.MethodGet, "/", nil)
	reqA1.Header.Set("X-OpenViking-Account", "A")
	r.ServeHTTP(recA1, reqA1)
	assert.Equal(t, http.StatusOK, recA1.Code)

	// Account A second request: rejected.
	recA2 := httptest.NewRecorder()
	reqA2 := httptest.NewRequest(http.MethodGet, "/", nil)
	reqA2.Header.Set("X-OpenViking-Account", "A")
	r.ServeHTTP(recA2, reqA2)
	assert.Equal(t, http.StatusTooManyRequests, recA2.Code)

	// Account B first request: still allowed (separate bucket).
	recB1 := httptest.NewRecorder()
	reqB1 := httptest.NewRequest(http.MethodGet, "/", nil)
	reqB1.Header.Set("X-OpenViking-Account", "B")
	r.ServeHTTP(recB1, reqB1)
	assert.Equal(t, http.StatusOK, recB1.Code)
}

func TestRateLimitAllowsAfterRefill(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RateLimit(100, 1)) // 100 rps -> ~10ms per token
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusOK, rec1.Code)

	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusTooManyRequests, rec2.Code)

	// Wait long enough for a token to refill.
	time.Sleep(30 * time.Millisecond)

	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusOK, rec3.Code)
}

func TestRateLimitConcurrentSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RateLimit(10, 5))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	var wg sync.WaitGroup
	var ok, limited atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			switch rec.Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	require.GreaterOrEqual(t, ok.Load(), int32(1), "at least one request should pass")
	require.GreaterOrEqual(t, limited.Load(), int32(1), "at least one request should be limited")
	total := ok.Load() + limited.Load()
	assert.Equal(t, int32(50), total, "all requests should be accounted for")
}
