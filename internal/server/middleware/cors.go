package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORSConfig configures the CORS middleware. It mirrors config.CORSConfig
// but lives in the middleware package so callers can construct middleware
// without importing config (useful in tests).
type CORSConfig struct {
	// AllowOrigins is the list of accepted origin values. A single "*"
	// entry allows any origin. When AllowCredentials is true, "*" is
	// replaced by the request Origin echo.
	AllowOrigins []string
	// AllowMethods is the set of allowed HTTP methods.
	AllowMethods []string
	// AllowHeaders is the set of allowed request headers.
	AllowHeaders []string
	// ExposeHeaders is the set of response headers the browser may read.
	ExposeHeaders []string
	// AllowCredentials controls whether cookies / Authorization cross-origin
	// requests are permitted.
	AllowCredentials bool
	// MaxAge is the preflight cache duration in seconds.
	MaxAge int
}

// CORS implements Cross-Origin Resource Sharing. OPTIONS requests are
// answered with 204 No Content and the CORS preflight headers. Other
// methods get the CORS allow-* headers attached and pass through to the
// next handler.
//
// When AllowOrigins is empty or contains "*", any origin is accepted
// (echoed back when AllowCredentials is true, "*" otherwise).
func CORS(cfg CORSConfig) gin.HandlerFunc {
	allow := normalizeCORS(cfg)
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		allowedOrigin := allow.match(origin)
		if allowedOrigin == "" {
			// No matching origin; for preflight return 403, for actual
			// request just don't add headers and let the browser reject.
			if c.Request.Method == http.MethodOptions {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			c.Next()
			return
		}
		c.Header("Access-Control-Allow-Origin", allowedOrigin)
		if allow.credentials {
			c.Header("Access-Control-Allow-Credentials", "true")
		}
		if len(allow.exposeHeaders) > 0 {
			c.Header("Access-Control-Expose-Headers", strings.Join(allow.exposeHeaders, ", "))
		}
		// Vary: Origin so caches differentiate by origin.
		c.Header("Vary", "Origin")
		if c.Request.Method == http.MethodOptions {
			if len(allow.methods) > 0 {
				c.Header("Access-Control-Allow-Methods", strings.Join(allow.methods, ", "))
			}
			if len(allow.headers) > 0 {
				c.Header("Access-Control-Allow-Headers", strings.Join(allow.headers, ", "))
			}
			if allow.maxAge > 0 {
				c.Header("Access-Control-Max-Age", strconv.Itoa(allow.maxAge))
			}
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// normalizedCORS is the resolved CORS policy after defaults are applied.
type normalizedCORS struct {
	allowOrigins  map[string]bool
	allowAll      bool
	credentials   bool
	methods       []string
	headers       []string
	exposeHeaders []string
	maxAge        int
}

func normalizeCORS(cfg CORSConfig) normalizedCORS {
	n := normalizedCORS{
		credentials:   cfg.AllowCredentials,
		methods:       cfg.AllowMethods,
		headers:       cfg.AllowHeaders,
		exposeHeaders: cfg.ExposeHeaders,
		maxAge:        cfg.MaxAge,
	}
	if len(n.methods) == 0 {
		n.methods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions}
	}
	if len(n.headers) == 0 {
		n.headers = []string{"Content-Type", "Authorization", "X-Request-ID", "X-OpenViking-Account", "X-OpenViking-User"}
	}
	n.allowOrigins = make(map[string]bool, len(cfg.AllowOrigins))
	for _, o := range cfg.AllowOrigins {
		if o == "*" {
			n.allowAll = true
			continue
		}
		n.allowOrigins[o] = true
	}
	return n
}

// match returns the value to send back in Access-Control-Allow-Origin, or
// empty string if the origin is not allowed.
func (n normalizedCORS) match(origin string) string {
	if origin == "" {
		// Same-origin request; no CORS headers needed but we still allow
		// the request through (browsers do not enforce CORS for same-origin).
		if n.allowAll {
			return "*"
		}
		return ""
	}
	if n.allowAll {
		if n.credentials {
			// Cannot use "*" with credentials; echo origin.
			return origin
		}
		return "*"
	}
	if n.allowOrigins[origin] {
		return origin
	}
	return ""
}
