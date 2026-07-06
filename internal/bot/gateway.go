// Package bot hosts the vikingbot CLI plus the HTTP gateway that bridges
// the ctxhub-server /bot/v1 surface to the multi-channel runtime.
//
// Gateway is the HTTP boundary implemented by NewGateway. It satisfies
// routers.BotService (six gin handlers) without importing the routers
// package — Go's structural typing wires it in app.go via deps.Bot =
// bot.NewGateway(). Per-channel adapters (ChannelAdapter) and an
// optional SessionStore are injected by callers (cmd/ctxhub-server)
// so the gateway starts with no channels and still serves /bot/v1/health.
package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// ChannelAdapter is the per-channel webhook + send boundary the Gateway
// dispatches to. Implementations live alongside the channel runtime
// (internal/bot/channels); the gateway stays decoupled so it can be
// constructed without importing every channel SDK. The HTTP gateway
// concerns itself with webhook fan-in and outbound dispatch; the
// long-running Start loop is owned by the bot process, not the server.
type ChannelAdapter interface {
	// Name returns the channel key (e.g. "qq", "feishu", "websocket").
	Name() string
	// HandleWebhook parses the platform-specific inbound payload and
	// dispatches it to the agent loop. The payload is the raw request
	// body; the adapter is responsible for signature verification.
	HandleWebhook(ctx context.Context, payload []byte) error
	// Send delivers an outbound text message to a chat on this channel.
	Send(ctx context.Context, chatID, text string) error
}

// SessionStore is the bot session introspection surface the Gateway uses
// for ListSessions / GetSession. It is a small interface so the gateway
// can be constructed without importing internal/session (which would
// couple the HTTP boundary to the store's internal types). Callers wire
// a concrete adapter via WithSessions.
type SessionStore interface {
	ListSessions(ctx context.Context) ([]SessionSummary, error)
	GetSession(ctx context.Context, id string) (*SessionSummary, error)
}

// SessionSummary is the JSON-serializable session view returned by the
// gateway. It carries only the fields the HTTP surface needs; the
// underlying domain.Session is not exposed directly to keep the
// gateway's response shape stable across store implementations.
type SessionSummary struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Channel string `json:"channel,omitempty"`
}

// Gateway is the HTTP boundary for vikingbot. It implements the six
// routers.BotService methods (HandleWebhook, SendMessage, ListSessions,
// GetSession, ListChannels, Health) by delegating to per-channel
// adapters and an optional session store. Constructed with no channels
// by default; callers register channels via RegisterChannel.
//
// When no channels are configured the gateway still serves /bot/v1/*:
// Health and ListChannels return 200 with an empty channel set, while
// per-channel routes (HandleWebhook, SendMessage) return 404 for the
// unknown channel. This lets ctxhub-server boot end-to-end without
// a configured bot runtime.
type Gateway struct {
	mu       sync.RWMutex
	channels map[string]ChannelAdapter
	sessions SessionStore
}

// NewGateway returns an empty Gateway. Callers register channels via
// RegisterChannel and attach a session store via WithSessions.
func NewGateway() *Gateway {
	return &Gateway{channels: make(map[string]ChannelAdapter)}
}

// RegisterChannel binds a ChannelAdapter under its Name(). Re-registering
// the same name replaces the prior adapter. Nil adapters are ignored.
func (g *Gateway) RegisterChannel(c ChannelAdapter) {
	if c == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.channels[c.Name()] = c
}

// WithSessions attaches a SessionStore. Returns the receiver for chaining.
func (g *Gateway) WithSessions(s SessionStore) *Gateway {
	g.sessions = s
	return g
}

// HandleWebhook processes an inbound webhook payload from a channel.
// The :channel path parameter selects the adapter (qq, feishu, slack,
// dingtalk, telegram, websocket, ...); the request body is the
// platform-specific payload. Unknown channels return 404
// RESOURCE_NOT_FOUND; payload read errors return 422 VALIDATION_FAILED;
// adapter errors return 500 INTERNAL_ERROR.
func (g *Gateway) HandleWebhook(c *gin.Context) {
	channel := c.Param("channel")
	adapter, ok := g.getChannel(channel)
	if !ok {
		abortWithError(c, domain.NewAppError(domain.CodeResourceNotFound, http.StatusNotFound, fmt.Sprintf("bot: unknown channel %q", channel)))
		return
	}
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		abortWithError(c, domain.Wrap(domain.CodeValidationFailed, http.StatusBadRequest, err))
		return
	}
	if err := adapter.HandleWebhook(c.Request.Context(), payload); err != nil {
		abortWithError(c, domain.Wrap(domain.CodeInternalError, http.StatusInternalServerError, err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"received": true,
		"channel":  channel,
	})
}

// SendMessage dispatches an outbound message to a channel. The request
// body carries channel, chat_id, and text. Missing fields return 422
// VALIDATION_FAILED; unknown channel returns 404 RESOURCE_NOT_FOUND;
// adapter errors return 500 INTERNAL_ERROR.
func (g *Gateway) SendMessage(c *gin.Context) {
	var req struct {
		Channel string `json:"channel"`
		ChatID  string `json:"chat_id"`
		Text    string `json:"text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		abortWithError(c, domain.Wrap(domain.CodeValidationFailed, http.StatusUnprocessableEntity, err))
		return
	}
	adapter, ok := g.getChannel(req.Channel)
	if !ok {
		abortWithError(c, domain.NewAppError(domain.CodeResourceNotFound, http.StatusNotFound, fmt.Sprintf("bot: unknown channel %q", req.Channel)))
		return
	}
	if err := adapter.Send(c.Request.Context(), req.ChatID, req.Text); err != nil {
		abortWithError(c, domain.Wrap(domain.CodeInternalError, http.StatusInternalServerError, err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"sent":    true,
		"channel": req.Channel,
		"chat_id": req.ChatID,
	})
}

// ListSessions returns active bot sessions. When no session store is
// wired the response is an empty list (the gateway is healthy; the
// caller just hasn't configured session introspection).
func (g *Gateway) ListSessions(c *gin.Context) {
	if g.sessions == nil {
		c.JSON(http.StatusOK, gin.H{"sessions": []any{}})
		return
	}
	list, err := g.sessions.ListSessions(c.Request.Context())
	if err != nil {
		abortWithError(c, domain.Wrap(domain.CodeInternalError, http.StatusInternalServerError, err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": list})
}

// GetSession returns a single bot session by :id. Returns 404
// RESOURCE_NOT_FOUND when no session store is wired or the session
// does not exist.
func (g *Gateway) GetSession(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		abortWithError(c, domain.Wrap(domain.CodeValidationFailed, http.StatusBadRequest, fmt.Errorf("bot: missing session id")))
		return
	}
	if g.sessions == nil {
		abortWithError(c, domain.NewAppError(domain.CodeResourceNotFound, http.StatusNotFound, fmt.Sprintf("bot: session %q not found", id)))
		return
	}
	sess, err := g.sessions.GetSession(c.Request.Context(), id)
	if err != nil {
		abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, http.StatusNotFound, err))
		return
	}
	c.JSON(http.StatusOK, sess)
}

// ListChannels returns the registered channel names. The order is
// arbitrary (map iteration); callers that need a stable order should
// sort the response client-side.
func (g *Gateway) ListChannels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"channels": g.channelNames(),
	})
}

// Health reports the bot gateway health. The gateway is healthy when
// it is reachable; the response includes the registered channel count
// so operators can spot a misconfigured runtime at a glance.
func (g *Gateway) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"channels": len(g.channelNames()),
	})
}

// Chat handles POST /bot/v1/chat — send a message to the vikingbot agent
// and return a single response. The vikingbot agent runtime lives in a
// separate process (cmd/vikingbot); the gateway does not embed an agent,
// so this endpoint returns 501 UNSUPPORTED until an agent runtime is
// injected via WithAgent (TODO: P9). The route is registered so web-studio's
// playground chat input doesn't 404 — callers get a structured error
// envelope they can surface as "bot not configured".
func (g *Gateway) Chat(c *gin.Context) {
	abortWithError(c, domain.NewAppError(domain.CodeUnsupported, http.StatusNotImplemented,
		"bot: chat runtime not configured"))
}

// ChatStream handles POST /bot/v1/chat/stream — Server-Sent Events stream
// for a chat turn. Sets the SSE headers before returning 501 so the response
// shape matches the streaming contract; the error middleware still renders
// the structured envelope as the body's first (and only) event.
func (g *Gateway) ChatStream(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	abortWithError(c, domain.NewAppError(domain.CodeUnsupported, http.StatusNotImplemented,
		"bot: chat runtime not configured"))
}

// Feedback handles POST /bot/v1/feedback — submit a feedback payload
// (thumbs-up/down, free-text) for a prior chat turn. The vikingbot
// feedback store is not wired into the gateway; the route is registered
// so the SDK call doesn't 404. Returns 501 UNSUPPORTED.
func (g *Gateway) Feedback(c *gin.Context) {
	abortWithError(c, domain.NewAppError(domain.CodeUnsupported, http.StatusNotImplemented,
		"bot: feedback store not configured"))
}

// getChannel returns the adapter registered under name, or false. Safe
// for concurrent use.
func (g *Gateway) getChannel(name string) (ChannelAdapter, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	a, ok := g.channels[name]
	return a, ok
}

// channelNames returns the registered channel names in arbitrary order.
func (g *Gateway) channelNames() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, 0, len(g.channels))
	for name := range g.channels {
		out = append(out, name)
	}
	return out
}

// abortWithError attaches a *domain.AppError to the request context and
// aborts the chain. The server's errorMiddleware (wired in BuildApp on
// the engine) renders the structured envelope. This mirrors
// routers.abortWithError without importing the routers package (which
// would create a cycle: routers imports bot via deps.Bot, so bot must
// not import routers).
func abortWithError(c *gin.Context, err *domain.AppError) {
	_ = c.Error(err)
	c.Abort()
}
