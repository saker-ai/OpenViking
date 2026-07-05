package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCORSEngine(cfg CORSConfig) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS(cfg))
	r.GET("/data", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

func TestCORSPreflightReturns204(t *testing.T) {
	r := newCORSEngine(CORSConfig{
		AllowOrigins: []string{"https://example.com"},
		AllowMethods: []string{"GET", "POST"},
		AllowHeaders: []string{"Content-Type"},
		MaxAge:       600,
	})

	req := httptest.NewRequest(http.MethodOptions, "/data", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "https://example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, POST", rec.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "Content-Type", rec.Header().Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "600", rec.Header().Get("Access-Control-Max-Age"))
	assert.Equal(t, "Origin", rec.Header().Get("Vary"))
}

func TestCORSEchoesOriginOnWildcard(t *testing.T) {
	r := newCORSEngine(CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET"},
		AllowHeaders:     []string{"Content-Type"},
		AllowCredentials: true,
	})

	req := httptest.NewRequest(http.MethodOptions, "/data", nil)
	req.Header.Set("Origin", "https://caller.example")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	// With credentials, "*" cannot be used; origin must be echoed.
	assert.Equal(t, "https://caller.example", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Credentials"))
}

func TestCORSEchoesAllowOriginWildcard(t *testing.T) {
	r := newCORSEngine(CORSConfig{
		AllowOrigins: []string{"*"},
	})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Origin", "https://caller.example")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORSDeniesForeignOriginPreflight(t *testing.T) {
	r := newCORSEngine(CORSConfig{
		AllowOrigins: []string{"https://allowed.example"},
	})

	req := httptest.NewRequest(http.MethodOptions, "/data", nil)
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORSSameOriginNoHeaders(t *testing.T) {
	r := newCORSEngine(CORSConfig{
		AllowOrigins: []string{"https://allowed.example"},
	})

	// No Origin header -> same-origin request from the browser's view.
	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORSExposeHeaders(t *testing.T) {
	r := newCORSEngine(CORSConfig{
		AllowOrigins:  []string{"*"},
		ExposeHeaders: []string{"X-Request-ID", "X-Process-Time"},
	})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Origin", "https://caller.example")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "X-Request-ID, X-Process-Time", rec.Header().Get("Access-Control-Expose-Headers"))
}
