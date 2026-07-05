package observability

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func TestNewLoggerDefaultLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLoggerWith(config.OTELConfig{}, &buf)
	logger.Info("hello")
	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.Equal(t, "INFO", entry["level"])
	assert.Equal(t, "hello", entry["msg"])
}

func TestParseLogLevel(t *testing.T) {
	// For each configured level, log at INFO. INFO records are emitted
	// when the handler level is INFO or DEBUG; filtered at WARN/ERROR.
	cases := []struct {
		in        string
		wantEmits bool
	}{
		{"", true},
		{"info", true},
		{"debug", true},
		{"warn", false},
		{"warning", false},
		{"error", false},
		{"bogus", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			var buf bytes.Buffer
			logger := NewLoggerWith(config.OTELConfig{LogLevel: c.in}, &buf)
			logger.Info("x")
			if !c.wantEmits {
				assert.Empty(t, buf.String())
				return
			}
			var entry map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &entry), "output: %s", buf.String())
			assert.Equal(t, "INFO", entry["level"])
		})
	}
}

func TestLoggerMiddlewareOutput(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLoggerWith(config.OTELConfig{LogLevel: "info"}, &buf)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("request_id", "rid-123"); c.Next() })
	r.Use(LoggerMiddleware(logger))
	r.GET("/test", func(c *gin.Context) {
		RequestLogger(c).Info("hello")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set(identity.HeaderUser, "user1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry), "output: %s", buf.String())
	assert.Equal(t, "rid-123", entry["request_id"])
	assert.Equal(t, "acct", entry["account"])
	assert.Equal(t, "user1", entry["user"])
	assert.Equal(t, "INFO", entry["level"])
	assert.Equal(t, "hello", entry["msg"])
}

func TestLoggerMiddlewareWithoutIdentity(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLoggerWith(config.OTELConfig{}, &buf)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("request_id", "rid-456"); c.Next() })
	r.Use(LoggerMiddleware(logger))
	r.GET("/test", func(c *gin.Context) {
		RequestLogger(c).Info("hello")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.Equal(t, "rid-456", entry["request_id"])
	assert.NotContains(t, entry, "account")
}
