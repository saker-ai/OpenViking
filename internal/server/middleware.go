// Package server wires the OpenViking HTTP service: gin engine, middleware
// chains, health/readiness, pprof, prometheus, and the full router set.
package server

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/xid"
	"go.opentelemetry.io/otel/trace"
)

// HeaderRequestID is the canonical request-id response/request header.
const HeaderRequestID = "X-Request-ID"

// HeaderProcessTime is the response header carrying handler duration.
const HeaderProcessTime = "X-Process-Time"

// requestIDMiddleware assigns a request id when none is provided and
// stores it on the context + response header.
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(HeaderRequestID)
		if rid == "" {
			rid = xid.New().String()
		}
		c.Set("request_id", rid)
		c.Header(HeaderRequestID, rid)
		c.Next()
	}
}

// timingMiddleware records the handler duration in X-Process-Time.
func timingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		c.Header(HeaderProcessTime, time.Since(start).String())
	}
}

// traceIDMiddleware exposes the OTel trace id on the response for debugging.
func traceIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if sc := trace.SpanContextFromContext(c.Request.Context()); sc.IsValid() {
			c.Header("X-Trace-Id", sc.TraceID().String())
		}
		c.Next()
	}
}
