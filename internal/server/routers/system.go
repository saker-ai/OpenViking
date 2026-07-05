package routers

import (
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
	"github.com/saker-ai/ctxhub/internal/version"
)

// RegisterSystem wires /api/v1/system/* — version, runtime, health details,
// and SDK-compat endpoints for queue/consistency polling.
//
// SDK-compat aliases:
//   - POST /system/wait          — wait for queuefs to drain (best-effort)
//   - POST /system/consistency   — check resource indexing consistency
//
// Both return okResponse so the Go SDK's doJSON can unmarshal result.
func RegisterSystem(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/system")
	r.GET("/version", func(c *gin.Context) {
		c.JSON(http.StatusOK, version.Get())
	})
	r.GET("/info", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version":    version.Version,
			"commit":     version.Commit,
			"build_time": version.BuildTime,
			"go_version": runtime.Version(),
			"platform":   runtime.GOOS + "/" + runtime.GOARCH,
			"num_cpu":    runtime.NumCPU(),
		})
	})
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/ready", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	r.POST("/wait", systemWait(deps))
	r.POST("/consistency", systemConsistency(deps))
}

// systemWait handles POST /system/wait — best-effort wait for queuefs to
// drain pending tasks. When no queue is wired, returns immediately with
// processed=true. The Go SDK posts {"timeout":...} and expects result
// to carry a "processed" boolean.
func systemWait(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Timeout float64 `json:"timeout,omitempty"`
		}
		_ = c.ShouldBindJSON(&req)
		processed := true
		pending := 0
		if deps != nil && deps.Queue != nil {
			deadline := time.Now().Add(time.Duration(req.Timeout) * time.Second)
			if req.Timeout <= 0 {
				deadline = time.Now().Add(30 * time.Second)
			}
			for time.Now().Before(deadline) {
				if p, ok := deps.Queue.(interface{ Pending() int }); ok {
					pending = p.Pending()
					if pending == 0 {
						break
					}
				} else {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			processed = pending == 0
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"processed": processed,
			"pending":   pending,
		}))
	}
}

// systemConsistency handles POST /system/consistency — check that a
// resource's sidecar layers (abstract/overview/chunks) are present when
// the resource itself exists. Returns {consistent:bool, missing:[]}.
// The Go SDK posts {"uri":...} and expects result to carry "consistent".
func systemConsistency(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req struct {
			URI string `json:"uri,omitempty"`
		}
		_ = c.ShouldBindJSON(&req)
		if req.URI == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "uri is required"))
			return
		}
		p := resolveAccountPath(c, req.URI)
		_, err := deps.RAGFS.Stat(c.Request.Context(), p)
		if err != nil {
			c.JSON(http.StatusOK, okResponse(gin.H{
				"consistent": false,
				"missing":    []string{"resource"},
				"uri":        p,
			}))
			return
		}
		missing := make([]string, 0, 2)
		if has, _ := ragfs.HasAbstract(c.Request.Context(), deps.RAGFS, p); !has {
			missing = append(missing, "abstract")
		}
		if has, _ := ragfs.HasOverview(c.Request.Context(), deps.RAGFS, p); !has {
			missing = append(missing, "overview")
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"consistent": len(missing) == 0,
			"missing":    missing,
			"uri":        p,
		}))
	}
}

// resolveAccountPath scopes a caller-supplied URI to the caller's account
// prefix. Used by system endpoints that take a generic URI.
func resolveAccountPath(c *gin.Context, uri string) string {
	if uri == "" {
		uri = "/"
	}
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize("/accounts/" + id.Account + uri)
	}
	return ragfs.Normalize(uri)
}
