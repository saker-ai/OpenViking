package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBodyDumpDisabledByDefault(t *testing.T) {
	t.Cleanup(func() { SetBodyDump(false) })
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyDump())
	r.POST("/ingest", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewBufferString(`{"hello":"world"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	// No body_dump log line should be emitted when disabled.
	assert.False(t, strings.Contains(buf.String(), "body_dump"))
}

func TestBodyDumpLogsWhenEnabled(t *testing.T) {
	t.Cleanup(func() { SetBodyDump(false) })
	SetBodyDump(true)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("request_id", "req-test-1")
		c.Next()
	})
	r.Use(BodyDump())
	r.POST("/ingest", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewBufferString(`{"hello":"world"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	out := buf.String()
	assert.True(t, strings.Contains(out, "body_dump"), "log line should be emitted")
	// slog text handler escapes quotes inside the value as \".
	assert.True(t, strings.Contains(out, `req_body="{\"hello\":\"world\"}"`), "request body should be captured, got: %s", out)
	assert.True(t, strings.Contains(out, "req-test-1"), "request id should be on the log line")
}

func TestBodyDumpTruncatesAtMax(t *testing.T) {
	t.Cleanup(func() { SetBodyDump(false) })
	SetBodyDump(true)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyDump())
	r.POST("/ingest", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	big := bytes.Repeat([]byte("a"), 10_000)
	req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader(big))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	out := buf.String()
	assert.True(t, strings.Contains(out, "truncated"), "oversized body should be truncated")
}

func TestBodyDumpStillWorksWithoutBody(t *testing.T) {
	t.Cleanup(func() { SetBodyDump(false) })
	SetBodyDump(true)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyDump())
	r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.Contains(buf.String(), "body_dump"))
}
