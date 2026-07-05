package middleware

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// bodyDumpMax is the per-side body cap. 4 KiB keeps logs cheap and limits
// PII leakage while still capturing enough for debugging.
const bodyDumpMax = 4 * 1024

// bodyDumpEnabled is toggled by SetBodyDump or by the OV_LOG_BODY env var
// at construction time. It is read atomically on every request.
var bodyDumpEnabled atomic.Bool

// SetBodyDump toggles request/response body logging at runtime. When true,
// the body dump middleware captures up to 4 KiB of each side and emits a
// slog.Info line keyed by request id. PII-sensitive: keep this OFF in
// production unless actively debugging.
func SetBodyDump(enabled bool) {
	bodyDumpEnabled.Store(enabled)
}

// BodyDump logs request and response bodies when OV_LOG_BODY=1. The
// middleware is a no-op when disabled (the default) to avoid leaking
// PII in production logs.
//
// The middleware itself reads up to bodyDumpMax bytes from the request body
// (independent of whether the downstream handler does) so a snapshot is
// captured even for handlers that ignore the body. The original body is
// restored via io.NopCloser/io.MultiReader so downstream handlers see the
// full content.
func BodyDump() gin.HandlerFunc {
	// Initialize from env at construction time. Runtime toggling via
	// SetBodyDump overrides this.
	if v := os.Getenv("OV_LOG_BODY"); v == "1" || v == "true" {
		bodyDumpEnabled.Store(true)
	}
	return func(c *gin.Context) {
		if !bodyDumpEnabled.Load() || c.Request == nil || c.Request.Body == nil {
			c.Next()
			return
		}
		start := time.Now()
		// Read up to bodyDumpMax+1 bytes to detect truncation.
		cap := bodyDumpMax + 1
		buf := make([]byte, cap)
		n, _ := io.ReadFull(c.Request.Body, buf)
		var snapshot []byte
		truncated := false
		if n == cap {
			// We read the full cap (bodyDumpMax+1) — body is larger than cap.
			snapshot = buf[:bodyDumpMax]
			truncated = true
			// Restore the unread remainder + the snapshot we already consumed
			// so downstream handlers see the full body.
			c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), c.Request.Body))
		} else {
			snapshot = buf[:n]
			// Body fully consumed; restore just the snapshot.
			c.Request.Body = io.NopCloser(bytes.NewReader(snapshot))
		}

		c.Next()

		// After handler chain: emit one structured log line. We do not
		// capture response body in P12 because gin's ResponseWriter does
		// not expose a buffer; a full implementation lands with the
		// observability phase.
		slog.Info("body_dump",
			"request_id", c.GetString("request_id"),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"req_body", formatSnapshot(snapshot, truncated),
		)
	}
}

// formatSnapshot renders the captured body for logging. Non-UTF8 bytes are
// left as-is; slog's text handler will escape them.
func formatSnapshot(b []byte, truncated bool) string {
	s := string(b)
	if truncated {
		return s + "...[truncated]"
	}
	return s
}
