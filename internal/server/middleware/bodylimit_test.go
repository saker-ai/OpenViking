package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBodyLimitEngine(t *testing.T, max int64, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyLimit(max))
	if handler != nil {
		r.POST("/upload", handler)
	} else {
		// Default handler reads the body so MaxBytesReader's limit
		// actually triggers a 413 when the body is too large.
		r.POST("/upload", func(c *gin.Context) {
			_, err := io.Copy(io.Discard, c.Request.Body)
			if err != nil {
				c.AbortWithStatus(http.StatusRequestEntityTooLarge)
				return
			}
			c.Status(http.StatusOK)
		})
	}
	return r
}

func TestBodyLimitAllowsUnderLimit(t *testing.T) {
	r := newBodyLimitEngine(t, 1024, nil)
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewBufferString("hello"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBodyLimitRejectsOverLimit(t *testing.T) {
	r := newBodyLimitEngine(t, 8, nil)
	body := bytes.Repeat([]byte("x"), 100)
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestBodyLimitZeroDisables(t *testing.T) {
	r := newBodyLimitEngine(t, 0, nil)
	body := bytes.Repeat([]byte("x"), 100_000)
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
