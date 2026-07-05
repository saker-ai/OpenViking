package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/server/identity"
	"github.com/saker-ai/ctxhub/internal/server/routers"
)

// registerRoutes wires every router prefix listed in the design doc
// (section 7.9.1, 24 routers). Each router's concrete endpoints are
// implemented in its corresponding file under internal/server/routers/.
func registerRoutes(r *gin.Engine, deps *routers.Deps) {
	api := r.Group("/api/v1", identity.Middleware(), errorMiddleware())

	routers.RegisterAdmin(api, deps)
	routers.RegisterResources(api, deps)
	routers.RegisterFilesystem(api, deps)
	routers.RegisterContent(api, deps)
	routers.RegisterConsole(api, deps)
	routers.RegisterSearch(api, deps)
	routers.RegisterCode(api, deps)
	routers.RegisterRelations(api, deps)
	routers.RegisterSessions(api, deps)
	routers.RegisterSkills(api, deps)
	routers.RegisterSnapshot(api, deps)
	routers.RegisterStats(api, deps)
	routers.RegisterPack(api, deps)
	routers.RegisterPrivacyConfigs(api, deps)
	routers.RegisterDebug(api, deps)
	routers.RegisterObserver(api, deps)
	routers.RegisterTasks(api, deps)
	routers.RegisterUserSettings(api, deps)
	routers.RegisterWatches(api, deps)
	routers.RegisterSystem(api, deps)

	// OAuth 2.1 endpoints (DCR / token / revoke / JWKS / authorize) live
	// under /api/v1/oauth but OUTSIDE the identity middleware: RFC 6749
	// §3.1 requires the token endpoint to authenticate the client itself,
	// and DCR (RFC 7591) is by definition unauthenticated. The handlers
	// enforce their own auth (client credentials, PKCE) via fosite.
	oauthAPI := r.Group("/api/v1", errorMiddleware())
	routers.RegisterOAuth(oauthAPI, deps)

	// WebDAV — no identity middleware; auth handled inside.
	routers.RegisterWebDAV(r, deps)

	// Bot gateway — no identity middleware; bot tokens validated inside.
	routers.RegisterBot(r, deps)

	// MCP, pprof, metrics, studio are wired in BuildApp directly because
	// they require non-trivial third-party server setup.

	// Liveness/readiness without auth (k8s probes). /health is an alias
	// for /healthz so the Go SDK's Health() (which calls GET /health)
	// works against the Go server without modification.
	r.GET("/healthz", healthHandler)
	r.GET("/health", healthHandler)
	r.GET("/readyz", healthHandler)
	r.GET("/version", versionHandler)

	// Catch-all 404 with structured error.
	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, errorResponse{
			Error: errorBody{
				Code:    "RESOURCE_NOT_FOUND",
				Message: "no route for " + c.Request.URL.Path,
			},
		})
	})
}
