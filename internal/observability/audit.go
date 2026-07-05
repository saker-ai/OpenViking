// Package observability wires OpenTelemetry tracing, Prometheus metrics,
// and structured logging. The tracer provider returned by InitTracer is
// shutdown-aware so callers can flush spans on graceful exit.
package observability

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// AuditEvent records a single mutating request for compliance/billing.
// Fields mirror the design doc usage_audit schema (section 7.13.3).
type AuditEvent struct {
	Timestamp time.Time      `json:"timestamp"`
	Account   string         `json:"account"`
	User      string         `json:"user,omitempty"`
	Action    string         `json:"action"`
	Resource  string         `json:"resource"`
	Status    int            `json:"status"`
	RequestID string         `json:"request_id"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// AuditSink consumes audit events. Implementations must be safe for
// concurrent use.
type AuditSink interface {
	Record(ctx context.Context, ev AuditEvent) error
}

// NewAuditSink returns an AuditSink that emits JSON to stderr.
func NewAuditSink() AuditSink {
	return NewAuditSinkFor(os.Stderr)
}

// NewAuditSinkFor returns an AuditSink writing JSON to w. Intended for
// tests that need to capture output.
func NewAuditSinkFor(w io.Writer) AuditSink {
	return &jsonAuditSink{
		logger: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{})),
	}
}

type jsonAuditSink struct {
	logger *slog.Logger
}

// Record writes ev as a structured JSON log line at INFO level.
func (s *jsonAuditSink) Record(ctx context.Context, ev AuditEvent) error {
	attrs := []slog.Attr{
		slog.String("timestamp", ev.Timestamp.Format(time.RFC3339Nano)),
		slog.String("account", ev.Account),
		slog.String("user", ev.User),
		slog.String("action", ev.Action),
		slog.String("resource", ev.Resource),
		slog.Int("status", ev.Status),
		slog.String("request_id", ev.RequestID),
	}
	for k, v := range ev.Metadata {
		attrs = append(attrs, slog.Any(k, v))
	}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit_event", attrs...)
	return nil
}

// isMutating reports whether method modifies server state.
func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// AuditMiddleware returns a gin middleware that records every mutating
// request (POST/PUT/PATCH/DELETE) to sink. Idempotent reads (GET/HEAD/
// OPTIONS) are skipped. The event is recorded after the handler chain
// completes so the response status is captured.
//
// Identity is resolved first from the request context (set by
// identity.Middleware for /api/v1 routes) and then from the canonical
// X-OpenViking-* headers (so /webdav and /bot paths are still audited).
func AuditMiddleware(sink AuditSink) gin.HandlerFunc {
	return func(c *gin.Context) {
		if sink == nil || !isMutating(c.Request.Method) {
			c.Next()
			return
		}
		c.Next()
		rid, _ := c.Get("request_id")
		ridStr, _ := rid.(string)
		id, _ := identity.FromContext(c.Request.Context())
		if id.IsEmpty() {
			id = identity.FromHeaders(c.Request.Header)
		}
		_ = sink.Record(c.Request.Context(), AuditEvent{
			Timestamp: time.Now().UTC(),
			Account:   id.Account,
			User:      id.User,
			Action:    c.Request.Method,
			Resource:  c.Request.URL.Path,
			Status:    c.Writer.Status(),
			RequestID: ridStr,
		})
	}
}
