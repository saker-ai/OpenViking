package routers

import (
	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// RegisterBot wires /bot/v1/* — vikingbot gateway. The bot runs as a
// separate process (vikingbot) but the server exposes HTTP endpoints
// under /bot/v1 for inbound webhook fan-in, outbound message dispatch,
// session introspection, and health. Bot identity is verified inside
// the handlers (bot tokens, not the server's API key), so the group
// sits outside the /api/v1 identity middleware chain.
//
// Routes:
//
//	POST /bot/v1/channels/:channel/webhook  — inbound webhook for a channel
//	POST /bot/v1/messages                   — send a message to a channel
//	GET  /bot/v1/sessions                   — list bot sessions
//	GET  /bot/v1/sessions/:id               — get a bot session
//	GET  /bot/v1/channels                   — list registered channels
//	GET  /bot/v1/health                     — bot gateway health check
//
// When deps.Bot is nil every route returns 501 UNSUPPORTED so the server
// still boots without a configured bot runtime (mirrors OAuth router).
func RegisterBot(r *gin.Engine, deps *Deps) {
	g := r.Group("/bot/v1")
	if deps == nil || deps.Bot == nil {
		g.POST("/channels/:channel/webhook", botUnsupported)
		g.POST("/messages", botUnsupported)
		g.GET("/sessions", botUnsupported)
		g.GET("/sessions/:id", botUnsupported)
		g.GET("/channels", botUnsupported)
		g.GET("/health", botUnsupported)
		return
	}
	bot := deps.Bot
	g.POST("/channels/:channel/webhook", bot.HandleWebhook)
	g.POST("/messages", bot.SendMessage)
	g.GET("/sessions", bot.ListSessions)
	g.GET("/sessions/:id", bot.GetSession)
	g.GET("/channels", bot.ListChannels)
	g.GET("/health", bot.Health)
}

// botUnsupported is the nil-safe fallback for /bot/v1/* routes. It
// attaches domain.ErrUnsupported (501) so the error middleware renders
// a structured UNSUPPORTED envelope.
func botUnsupported(c *gin.Context) {
	abortWithError(c, domain.ErrUnsupported)
}
