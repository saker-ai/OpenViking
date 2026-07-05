// Package middleware contains the engineering-infrastructure middleware
// wired into the OpenViking HTTP server: rate limiting, circuit breaker,
// request body limits, request timeout, auth, CORS, and body dump.
//
// All middleware here is gin.HandlerFunc. Constructors return functions
// that close over their own configuration so they can be composed in
// BuildApp without shared state.
package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodyLimit enforces a maximum request body size. Bodies larger than maxBytes
// trigger a 413 PAYLOAD_TOO_LARGE response. The reader is wrapped before
// the chain continues so downstream handlers can rely on the limit.
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes <= 0 {
			c.Next()
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}
