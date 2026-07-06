// Package routers contains the HTTP routers exposed by ctxhub-server.
// Each router is registered onto the /api/v1 group by its own Register<Name>
// function; concrete handler logic lives in the corresponding file.
//
// Deps is the service container passed to every router. Fields are nil-safe:
// routers nil-check before use so a partially-wired App still boots and
// returns 501 for un-implemented endpoints (see stub).
package routers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/auth/apikeys"
	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/ingest"
	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/models/rerank"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
	"github.com/saker-ai/ctxhub/internal/parse"
	"github.com/saker-ai/ctxhub/internal/queuefs"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/retrieve"
	"github.com/saker-ai/ctxhub/internal/session"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// Deps carries every service a router might need. Each field is a concrete
// type or interface from the corresponding internal package so routers can
// call real methods. Fields are nil-safe: routers must nil-check before use
// until the corresponding phase lands.
type Deps struct {
	Config *config.Config

	// RAGFS is the mountable router. May be a *ragfs.MountableFS or any
	// ragfs.FileSystem. nil in tests that don't exercise fs routes.
	RAGFS ragfs.FileSystem

	// VectorDB is the tenant-scoped collection adapter.
	VectorDB vectordb.CollectionAdapter

	// Queue is the queuefs server (memory or redis).
	Queue queuefs.QueueServer

	// Ingest is the ingest orchestrator.
	Ingest *ingest.Orchestrator

	// Retrieve is the hierarchical retriever.
	Retrieve retrieve.Retriever

	// Sessions is the session store.
	Sessions session.Store

	// MCP is the MCP server boundary.
	MCP MCPService

	// OAuth is the OAuth 2.1 handler boundary.
	OAuth OAuthService

	// Bot is the vikingbot gateway boundary. When nil, every /bot/v1/*
	// route returns 501 UNSUPPORTED so the server still boots without a
	// configured bot runtime. Implementations live outside the routers
	// package (the bot runtime is a separate process; a thin HTTP adapter
	// wraps it for webhook fan-in + outbound dispatch).
	Bot BotService

	// StudioFS is the http.FileSystem for web-studio assets, if
	// configured. When nil, /studio/* routes return 501 UNSUPPORTED.
	// BuildApp constructs it from cfg.Server.StudioPath via http.Dir;
	// callers that want to embed assets can substitute an embed.FS
	// wrapper that implements http.FileSystem.
	StudioFS http.FileSystem

	// APIKeys is the API key manager used by /admin/accounts/:id/api-keys.
	// When nil, those endpoints return 501 UNSUPPORTED. BuildApp
	// constructs it from cfg.Server.APIKeysPath; tests inject a
	// manager backed by a temp file.
	APIKeys *apikeys.Manager

	// Parse is the parse dispatcher (package-level functions wrapped in a
	// struct so it can be injected). nil means Dispatch falls back to the
	// package-level registry.
	Parse *ParseDispatcher

	// Models bundles the VLM, embedder, and reranker clients. Any field may
	// be nil when the corresponding provider is not configured; routers
	// that need a model must nil-check before use.
	Models ModelsContainer
}

// ModelsContainer bundles the provider clients used by retrieve / ingest /
// session pipelines. Tests inject stub implementations (vlm.Stub,
// embedder.LocalClient, rerank.LocalReranker) to avoid network calls.
type ModelsContainer struct {
	VLM      vlm.VLM
	Embedder embedder.Embedder
	Reranker rerank.Reranker
}

// ParseDispatcher is a thin wrapper around the package-level parse functions
// (RegisterAccessor, RegisterParser, Dispatch) so the registry can be
// injected via Deps. The zero value is usable; it forwards to the package
// registry.
type ParseDispatcher struct {
	// AccessorFactorys keyed by scheme, registered on first Dispatch call.
	// nil means use the package-level registry directly.
	accessorFactories map[string]parse.AccessorFactory
	parserFactories   map[string]parse.ParserFactory
}

// NewParseDispatcher constructs an empty ParseDispatcher.
func NewParseDispatcher() *ParseDispatcher {
	return &ParseDispatcher{
		accessorFactories: map[string]parse.AccessorFactory{},
		parserFactories:   map[string]parse.ParserFactory{},
	}
}

// RegisterAccessor forwards to parse.RegisterAccessor.
func (d *ParseDispatcher) RegisterAccessor(scheme string, f parse.AccessorFactory) {
	if d == nil {
		parse.RegisterAccessor(scheme, f)
		return
	}
	d.accessorFactories[scheme] = f
	parse.RegisterAccessor(scheme, f)
}

// RegisterParser forwards to parse.RegisterParser.
func (d *ParseDispatcher) RegisterParser(name string, f parse.ParserFactory) {
	if d == nil {
		parse.RegisterParser(name, f)
		return
	}
	d.parserFactories[name] = f
	parse.RegisterParser(name, f)
}

// Dispatch forwards to parse.Dispatch.
func (d *ParseDispatcher) Dispatch(ctx context.Context, source string, opts parse.AccessorOptions, popts parse.ParserOptions) (*parse.LocalResource, *parse.ParseResult, error) {
	return parse.Dispatch(ctx, source, opts, popts)
}

// MCPService marks the MCP server boundary. Implementations return an
// http.Handler that exposes the streamable-HTTP MCP transport (already
// wrapped with the configured auth middleware). Routers register it under
// /mcp. The Tools() slice is exposed for metadata endpoints (e.g. system
// info) and is not required for routing.
type MCPService interface {
	HTTPHandler() http.Handler
	Tools() []string
}

// OAuthService marks the OAuth 2.1 handler boundary. Implementations
// expose the per-route gin handlers so the OAuth router can delegate
// without depending on fosite directly.
type OAuthService interface {
	RegisterHandler(c *gin.Context)
	AuthorizeHandler(c *gin.Context)
	CallbackHandler(c *gin.Context)
	TokenHandler(c *gin.Context)
	RevokeHandler(c *gin.Context)
	JWKSHandler(c *gin.Context)
	AuthorizationServerMetadataHandler(c *gin.Context)
	ProtectedResourceMetadataHandler(c *gin.Context)
}

// BotService marks the vikingbot gateway boundary. Implementations expose
// per-route gin handlers so the bot router can delegate without depending
// on the bot runtime packages directly (mirrors OAuthService).
//
// The bot gateway is the unified HTTP entry point for inbound webhook
// fan-in (QQ / Feishu / Slack / etc.), outbound message dispatch, session
// introspection, and health. Bot identity is verified inside the handlers
// (bot tokens, not the server's API key) — the gateway sits outside the
// /api/v1 identity middleware chain.
//
// When Deps.Bot is nil, RegisterBot falls back to 501 UNSUPPORTED for
// every route so the server still boots without a configured bot.
type BotService interface {
	// HandleWebhook processes an inbound webhook payload from a channel.
	// The :channel path parameter selects the adapter (qq, feishu, slack,
	// dingtalk, telegram, ...); the request body is the platform-specific
	// payload.
	HandleWebhook(c *gin.Context)
	// SendMessage dispatches an outbound message to a channel. The
	// request body carries channel, chat_id, and text.
	SendMessage(c *gin.Context)
	// ListSessions returns active bot sessions.
	ListSessions(c *gin.Context)
	// GetSession returns a single bot session by :id.
	GetSession(c *gin.Context)
	// ListChannels returns the registered channels.
	ListChannels(c *gin.Context)
	// Health reports the bot gateway health.
	Health(c *gin.Context)
	// Chat sends a message to the vikingbot agent and returns a single
	// response. Used by web-studio's playground chat input.
	Chat(c *gin.Context)
	// ChatStream sends a message and returns an SSE stream of events.
	ChatStream(c *gin.Context)
	// Feedback submits a feedback payload for a prior chat turn.
	Feedback(c *gin.Context)
}
