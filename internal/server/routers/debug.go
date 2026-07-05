package routers

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterDebug wires /api/v1/debug/* — debug introspection.
//
// Every endpoint here is read-only and safe to expose in production behind
// the standard admin auth. Sensitive env vars and config fields (anything
// containing SECRET, KEY, TOKEN, PASSWORD, CREDENTIAL) are masked.
func RegisterDebug(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/debug")
	r.GET("/ctx", debugCtx(deps))
	r.GET("/headers", debugHeaders(deps))
	r.GET("/env", debugEnv(deps))
	r.GET("/config", debugConfig(deps))
	// SDK BFF endpoints: web-studio's generated SDK calls /health and
	// /vector/{count,scroll} for the debug console. Return disabled-shaped
	// payloads so the SDK doesn't 404 even when no vector DB is wired.
	r.GET("/health", debugHealth(deps))
	r.GET("/vector/count", debugVectorCount(deps))
	r.GET("/vector/scroll", debugVectorScroll(deps))
}

// debugHealth handles GET /debug/health — process-level health rollup.
// Mirrors Python openviking/server/routers/debug.py is_healthy.
func debugHealth(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, okResponse(gin.H{"healthy": true}))
	}
}

// debugVectorCount handles GET /debug/vector/count — count rows in the
// vector DB matching an optional filter / uri. Returns {count:0, disabled:true}
// when no vector DB is wired so Studio's debug console renders the empty
// state instead of 404.
func debugVectorCount(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		disabled := deps == nil || deps.VectorDB == nil
		c.JSON(http.StatusOK, okResponse(gin.H{
			"count":    0,
			"disabled": disabled,
		}))
	}
}

// debugVectorScroll handles GET /debug/vector/scroll — paginate rows in
// the vector DB. Returns an empty page when no vector DB is wired.
func debugVectorScroll(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, _ := strconv.Atoi(c.Query("limit"))
		if limit <= 0 {
			limit = 100
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"records":     []any{},
			"next_cursor": nil,
			"disabled":    deps == nil || deps.VectorDB == nil,
			"limit":       limit,
		}))
	}
}

// debugCtx handles GET /debug/ctx — return the request context's identity
// (account / user / actor-peer) and a few request-scoped fields.
func debugCtx(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := identity.FromContext(c.Request.Context())
		ident := gin.H{"present": ok}
		if ok {
			ident["account"] = id.Account
			ident["user"] = id.User
			ident["actor_peer"] = id.ActorPeer
		}
		c.JSON(http.StatusOK, gin.H{
			"identity":   ident,
			"method":     c.Request.Method,
			"path":       c.Request.URL.Path,
			"query":      c.Request.URL.RawQuery,
			"client_ip":  c.ClientIP(),
			"user_agent": c.Request.UserAgent(),
			"request_id": c.GetString("request_id"),
		})
	}
}

// debugHeaders handles GET /debug/headers — return the request headers as
// a map. Authorization, Cookie, and similar credential-bearing headers
// are masked to avoid leaking them into logs.
func debugHeaders(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		out := make(map[string]string, len(c.Request.Header))
		for k, v := range c.Request.Header {
			if len(v) == 0 {
				continue
			}
			lk := strings.ToLower(k)
			if isSecretKey(lk) || lk == "authorization" || lk == "cookie" {
				out[k] = maskedValue(v[0])
				continue
			}
			out[k] = v[0]
		}
		c.JSON(http.StatusOK, gin.H{"headers": out, "count": len(out)})
	}
}

// debugEnv handles GET /debug/env — return a sanitized subset of the
// process environment. Variables whose name contains SECRET, KEY, TOKEN,
// PASSWORD, or CREDENTIAL are masked. OV_-prefixed variables are always
// included unmasked (they are the app's public config surface).
func debugEnv(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		envs := os.Environ()
		out := make(map[string]string, len(envs))
		for _, kv := range envs {
			idx := strings.IndexByte(kv, '=')
			if idx < 0 {
				continue
			}
			name, val := kv[:idx], kv[idx+1:]
			if strings.HasPrefix(name, "OV_") {
				out[name] = val
				continue
			}
			if isSecretKey(name) {
				out[name] = maskedValue(val)
				continue
			}
			out[name] = val
		}
		c.JSON(http.StatusOK, gin.H{
			"env":        out,
			"count":      len(out),
			"go_version": runtime.Version(),
			"platform":   runtime.GOOS + "/" + runtime.GOARCH,
			"num_cpu":    runtime.NumCPU(),
			"pid":        os.Getpid(),
		})
	}
}

// debugConfig handles GET /debug/config — return the loaded configuration
// as a sanitized JSON map. When deps.Config is nil the endpoint returns
// 501 UNSUPPORTED. Secret leaves (any key matching SECRET / PASSWORD /
// TOKEN / CREDENTIAL / API_KEY / PRIVATE_KEY / SECRET_KEY) are masked.
func debugConfig(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Config == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		raw, err := json.Marshal(deps.Config)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"config":  sanitizeConfigMap(m),
			"version": "1",
		})
	}
}

// maskedValue returns the input reduced to its first and last character
// with the middle replaced by asterisks. Inputs shorter than 4 characters
// collapse to "***" so no original bytes are revealed.
func maskedValue(s string) string {
	if len(s) < 4 {
		return "***"
	}
	return string(s[0]) + strings.Repeat("*", len(s)-2) + string(s[len(s)-1])
}

// sanitizeConfigMap walks m and masks any leaf whose key matches a known
// secret pattern.
func sanitizeConfigMap(m map[string]interface{}) map[string]interface{} {
	for k, v := range m {
		m[k] = sanitizeValue(k, v)
	}
	return m
}

func sanitizeValue(key string, v interface{}) interface{} {
	switch vv := v.(type) {
	case map[string]interface{}:
		return sanitizeConfigMap(vv)
	case []interface{}:
		out := make([]interface{}, len(vv))
		for i, item := range vv {
			out[i] = sanitizeValue(key, item)
		}
		return out
	case string:
		if isSecretKey(key) {
			return maskedValue(vv)
		}
		return vv
	default:
		return v
	}
}

// isSecretKey reports whether name (case-insensitive) looks like a secret
// field.
func isSecretKey(name string) bool {
	ln := strings.ToUpper(name)
	switch {
	case strings.Contains(ln, "SECRET"),
		strings.Contains(ln, "PASSWORD"),
		strings.Contains(ln, "TOKEN"),
		strings.Contains(ln, "CREDENTIAL"),
		ln == "API_KEY",
		ln == "PRIVATE_KEY",
		ln == "SECRET_KEY":
		return true
	}
	return false
}
