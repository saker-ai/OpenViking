package routers

import (
	"github.com/gin-gonic/gin"
)

// RegisterOAuth wires /api/v1/oauth/* and the .well-known endpoints.
//
// When deps.OAuth is nil (OAuth not enabled in config), every endpoint
// returns a 501 stub. When OAuth is enabled, the real fosite-backed
// handlers in internal/server/oauth take over.
//
// The .well-known endpoints live at the engine root, not under /api/v1,
// so the .well-known routes are registered on the engine via the
// RegisterWellKnown function (called from BuildApp when OAuth is wired).
func RegisterOAuth(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/oauth")
	if deps == nil || deps.OAuth == nil {
		r.POST("/register", stub("oauth.register"))
		r.GET("/authorize", stub("oauth.authorize"))
		r.POST("/authorize", stub("oauth.authorize_post"))
		r.GET("/callback", stub("oauth.callback"))
		r.POST("/token", stub("oauth.token"))
		r.POST("/revoke", stub("oauth.revoke"))
		r.GET("/keys", stub("oauth.jwks"))
		return
	}

	oauth := deps.OAuth
	r.POST("/register", oauth.RegisterHandler)
	r.GET("/authorize", oauth.AuthorizeHandler)
	r.POST("/authorize", oauth.AuthorizeHandler)
	r.GET("/callback", oauth.CallbackHandler)
	r.POST("/token", oauth.TokenHandler)
	r.POST("/revoke", oauth.RevokeHandler)
	r.GET("/keys", oauth.JWKSHandler)
}

// RegisterWellKnown wires the RFC 8414 / RFC 9728 metadata endpoints at
// the engine root. Called from BuildApp only when OAuth is enabled.
func RegisterWellKnown(r *gin.Engine, deps *Deps) {
	if deps == nil || deps.OAuth == nil {
		return
	}
	oauth := deps.OAuth
	r.GET("/.well-known/oauth-authorization-server", oauth.AuthorizationServerMetadataHandler)
	r.GET("/.well-known/oauth-protected-resource", oauth.ProtectedResourceMetadataHandler)
}
