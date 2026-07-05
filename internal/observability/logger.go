// Package observability wires OpenTelemetry tracing, Prometheus metrics,
// and structured logging. The tracer provider returned by InitTracer is
// shutdown-aware so callers can flush spans on graceful exit.
package observability

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// loggerKey is the gin-context key holding the request-scoped *slog.Logger.
type loggerKey struct{}

// NewLogger builds a slog.Logger that emits JSON to stderr at the level
// configured by cfg.OTEL.LogLevel (debug/info/warn/error). Empty LogLevel
// defaults to info.
func NewLogger(cfg config.OTELConfig) *slog.Logger {
	return NewLoggerWith(cfg, os.Stderr)
}

// NewLoggerWith builds a slog.Logger writing JSON to w. Intended for
// tests that need to capture output.
func NewLoggerWith(cfg config.OTELConfig, w io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	})
	return slog.New(handler)
}

// parseLogLevel maps the configured string to a slog.Level. Unknown
// values fall back to info.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LoggerMiddleware returns a gin middleware that injects a request-scoped
// *slog.Logger into the gin context. The logger carries the request_id
// (set by the request-id middleware) and identity labels (account/user)
// read from the canonical X-OpenViking-* headers via identity.FromHeaders.
// Subsequent handlers retrieve it via RequestLogger.
func LoggerMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		lg := logger
		if rid, ok := c.Get("request_id"); ok {
			if ridStr, ok := rid.(string); ok && ridStr != "" {
				lg = lg.With("request_id", ridStr)
			}
		}
		id := identity.FromHeaders(c.Request.Header)
		if !id.IsEmpty() {
			lg = lg.With("account", id.Account)
			if id.User != "" {
				lg = lg.With("user", id.User)
			}
		}
		c.Set("logger", lg)
		c.Next()
	}
}

// RequestLogger returns the request-scoped logger from the gin context,
// falling back to slog.Default() when none is set (e.g. non-gin paths).
func RequestLogger(c *gin.Context) *slog.Logger {
	if v, ok := c.Get("logger"); ok {
		if lg, ok := v.(*slog.Logger); ok && lg != nil {
			return lg
		}
	}
	return slog.Default()
}
