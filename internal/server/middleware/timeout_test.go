package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimeoutAllowsFastHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Timeout(100 * time.Millisecond))
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

func TestTimeout504OnSlowHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Timeout(20 * time.Millisecond))
	r.GET("/", func(c *gin.Context) {
		// Slow handler that ignores context cancellation.
		select {
		case <-time.After(200 * time.Millisecond):
			c.String(http.StatusOK, "slow")
		case <-c.Request.Context().Done():
			// Return without writing so the middleware's 504 is the
			// only response emitted.
			return
		}
	})

	rec := httptest.NewRecorder()
	start := time.Now()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	elapsed := time.Since(start)

	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.Contains(t, rec.Body.String(), "GATEWAY_TIMEOUT")
	assert.Less(t, elapsed, 150*time.Millisecond, "middleware should short-circuit well before slow handler completes")
	assert.NotEmpty(t, rec.Header().Get("X-Timeout"))
}

func TestTimeoutZeroDisables(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Timeout(0))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestTimeoutPreservesHandlerStatusOnSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Timeout(100 * time.Millisecond))
	r.GET("/teapot", func(c *gin.Context) { c.String(http.StatusTeapot, "i am a teapot") })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/teapot", nil))
	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, "i am a teapot", rec.Body.String())
}
