// Package routers contains the 24 HTTP routers listed in design doc
// section 7.9.1. Each router is registered onto the /api/v1 group by
// its own Register<Name> function; concrete handler logic lands in
// later phases (P3-P9).
package routers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// stub returns a gin.HandlerFunc that responds with 501 Not Implemented
// and a structured error body. Routers use it for endpoints whose
// service deps are not yet wired.
func stub(name string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": gin.H{
				"code":    "NOT_IMPLEMENTED",
				"message": name + " not yet implemented",
			},
		})
	}
}

// groupFunc is a tiny helper to register a router group with a path.
type groupFunc func(*gin.RouterGroup)
