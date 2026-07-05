package identity

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Middleware extracts the X-OpenViking-Account/User/Actor-Peer headers and
// stores the resulting Identifier in the request context. Requests missing
// the Account header are rejected with 401; User and ActorPeer are optional.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := FromHeaders(c.Request.Header)
		if id.Account == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{
					"code":    "UNAUTHORIZED",
					"message": "missing X-OpenViking-Account header",
				},
			})
			return
		}
		c.Request = c.Request.WithContext(WithIdentity(c.Request.Context(), id))
		c.Next()
	}
}
