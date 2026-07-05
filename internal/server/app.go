// Package server wires the OpenViking HTTP service: gin engine, middleware
// chains, health/readiness, pprof, prometheus, and the full router set.
//
// App is the top-level runtime container returned to cmd/openviking-server.
// BuildApp constructs it from a *config.Config; the returned cleanup func
// releases every resource acquired during construction in reverse order.
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/saker-ai/ctxhub/internal/bot"
	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/crypto"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ingest"
	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/models/rerank"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
	"github.com/saker-ai/ctxhub/internal/observability"
	"github.com/saker-ai/ctxhub/internal/queuefs"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/localfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/s3fs"
	"github.com/saker-ai/ctxhub/internal/retrieve"
	"github.com/saker-ai/ctxhub/internal/auth/apikeys"
	"github.com/saker-ai/ctxhub/internal/server/mcp"
	"github.com/saker-ai/ctxhub/internal/server/middleware"
	"github.com/saker-ai/ctxhub/internal/server/oauth"
	"github.com/saker-ai/ctxhub/internal/server/routers"
	"github.com/saker-ai/ctxhub/internal/session"
	"github.com/saker-ai/ctxhub/internal/version"
	"github.com/saker-ai/ctxhub/internal/vectordb"

	// Side-effect imports register parse accessors and parsers into the
	// package-level registry via init(). The ParseDispatcher in Deps
	// forwards to that registry.
	_ "github.com/saker-ai/ctxhub/internal/parse/accessors"
	_ "github.com/saker-ai/ctxhub/internal/parse/parsers"
	_ "github.com/saker-ai/ctxhub/internal/parse/parsers/code"
	_ "github.com/saker-ai/ctxhub/internal/parse/parsers/media"
)

// App is the runtime container built from a Config.
type App struct {
	cfg    *config.Config
	engine *gin.Engine
	deps   *routers.Deps
	queue  QueueServer
}

// QueueServer is the minimal interface App needs from queuefs to drive
// graceful shutdown. queuefs.QueueServer satisfies it.
type QueueServer interface {
	Shutdown() error
	Start(context.Context) error
}

// noopQueueServer is the fallback when no queue backend is configured.
// It satisfies queuefs.QueueServer by no-op-ing every method.
type noopQueueServer struct{}

func (noopQueueServer) Start(context.Context) error      { return nil }
func (noopQueueServer) Shutdown() error                  { return nil }
func (noopQueueServer) Enqueue(*queuefs.Task) error      { return nil }
func (noopQueueServer) RegisterHandler(string, queuefs.HandlerFunc) {}

// BuildApp constructs an App from cfg and returns a cleanup func that
// releases acquired resources. The HTTP router is wired with health,
// version, metrics, pprof, and the full 24-router API surface.
func BuildApp(cfg *config.Config) (*App, func(), error) {
	if cfg == nil {
		return nil, nil, errNilConfig
	}
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	// Observability: structured logger, RED metrics, otel spans, audit.
	logger := observability.NewLogger(cfg.OTEL)
	metrics := observability.NewMetrics()
	auditSink := observability.NewAuditSink()
	serviceName := cfg.OTEL.ServiceName
	if serviceName == "" {
		serviceName = "openviking-server"
	}

	// Middleware chain (order matters — see design doc 7.9.2).
	engine.Use(
		requestIDMiddleware(),
		gin.Recovery(),
		middleware.CORS(middleware.CORSConfig{
			AllowOrigins:     cfg.Server.CORS.AllowOrigins,
			AllowMethods:     cfg.Server.CORS.AllowMethods,
			AllowHeaders:     cfg.Server.CORS.AllowHeaders,
			AllowCredentials: cfg.Server.CORS.AllowCredentials,
			MaxAge:           cfg.Server.CORS.MaxAge,
		}),
		otelgin.Middleware(serviceName),
		observability.LoggerMiddleware(logger),
		observability.MetricsMiddleware(metrics),
		observability.AuditMiddleware(auditSink),
		timingMiddleware(),
		traceIDMiddleware(),
		middleware.BodyLimit(cfg.Server.MaxUploadSize),
		middleware.Timeout(time.Duration(cfg.Server.RequestTimeout)*time.Second),
		middleware.Auth(middleware.AuthConfig{
			HashAlgo: cfg.Auth.APIKey.HashAlgo,
		}),
		middleware.RateLimit(defaultRateLimitRPS, defaultRateLimitBurst),
		middleware.BodyDump(),
		errorMiddleware(),
	)

	deps := &routers.Deps{Config: cfg}

	// Construct real services from cfg. Each constructor is nil-safe: when
	// the corresponding config block is empty or default, the service is
	// an in-memory or stub implementation so the server still boots in tests.
	ctx := context.Background()
	var queue queuefs.QueueServer = noopQueueServer{}
	var vdb vectordb.CollectionAdapter
	var err error

	cleanup := func() {
		if vdb != nil {
			_ = vdb.Close()
		}
		_ = queue.Shutdown()
	}

	// RAGFS mountable router. When cfg.RAGFS.Mounts is empty a memory
	// backend is mounted at "/" so the server is usable in tests without a
	// real filesystem.
	mnt := ragfs.NewMountableFS()
	if len(cfg.RAGFS.Mounts) == 0 {
		_ = mnt.Mount("/", memfs.New("default"))
	} else {
		for _, m := range cfg.RAGFS.Mounts {
			prefix := m.Path
			if prefix == "" {
				prefix = "/"
			}
			var backend ragfs.FileSystem
			switch m.Backend {
			case "memory", "":
				backend = memfs.New(m.Name)
			case "local":
				lfs, err := localfs.New(m.Name, m.Path)
				if err != nil {
					return nil, cleanup, fmt.Errorf("ragfs mount %s: %w", m.Name, err)
				}
				backend = lfs
			case "s3":
				// S3 mounts use the bucket as the path source; the mount
				// prefix is what we register. Credentials come from the
				// mount stanza (P6 will wire env-based credentials).
				backend = s3fs.New(m.Name, m.Endpoint, m.Bucket, m.Region, "", "", "", m.ReadOnly)
			default:
				return nil, cleanup, fmt.Errorf("ragfs mount %s: unknown backend %q", m.Name, m.Backend)
			}
			if err := mnt.Mount(prefix, backend); err != nil {
				return nil, cleanup, fmt.Errorf("ragfs mount %s: %w", m.Name, err)
			}
		}
	}
	deps.RAGFS = mnt

	// VectorDB adapter. The memory backend never fails; qdrant may fail
	// when its URL is missing — in that case we fall back to memory so the
	// server still boots and console routes return empty results rather
	// than 501.
	vdb, err = vectordb.NewAdapter(ctx, &cfg.VectorDB)
	if err != nil {
		vdb = vectordb.NewMemoryAdapter()
	}
	deps.VectorDB = vdb

	// Queuefs. Memory by default; Redis when configured.
	if cfg.Queue.Backend == "redis" && cfg.Queue.Redis.Addr != "" {
		queue = queuefs.NewRedisServer(cfg.Queue.Redis.Addr,
			queuefs.WithRedisPassword(cfg.Queue.Redis.Password),
			queuefs.WithRedisDB(cfg.Queue.Redis.DB),
		)
	} else {
		queue = queuefs.NewMemoryServer()
	}
	if err := queue.Start(ctx); err != nil {
		queue = noopQueueServer{}
	}
	deps.Queue = queue

	// Session store: in-memory by default. SQLite-backed store lands in a
	// later phase; for now MemoryStore covers tests and dev.
	sessionsStore := session.NewMemoryStore(session.StoreConfig{})
	deps.Sessions = sessionsStore

	// Models container. Provider is selected from cfg; stubs/local
	// backends remain the default so the server still boots in tests
	// without network access. Real providers (openai/volcengine/dashscope)
	// are constructed only when the corresponding cfg.Provider is set and
	// the APIKey is non-empty — tests MUST NOT make network calls.
	models := routers.ModelsContainer{
		VLM:      newVLM(cfg.VLM),
		Embedder: newEmbedder(cfg.Embedder),
		Reranker: newReranker(cfg.Rerank),
	}
	deps.Models = models

	// Retriever: hierarchical, wired with the vectordb adapter + ragfs.
	intent := retrieve.NewVLMIntentAnalyzer(models.VLM, cfg.VLM.Model)
	searcher := retrieve.NewDefaultHybridSearcher(vdb, models.Embedder, mnt)
	retriever := retrieve.NewHierarchicalRetriever(
		intent, searcher,
		retrieve.NewAdapterReranker(models.Reranker),
		models.Embedder, vdb, mnt,
		cfg.VectorDB.CollectionPrefix,
	)
	deps.Retrieve = retriever

	// Ingest orchestrator with an empty source registry; sources are
	// registered by callers (cmd/openviking-server) via orchestrator.Register.
	deps.Ingest = ingest.NewOrchestrator(nil, nil)

	// Parse dispatcher. The package-level registry is populated via the
	// side-effect imports above; the dispatcher is a thin wrapper.
	deps.Parse = routers.NewParseDispatcher()

	// OAuth 2.1 server (fosite). Constructed when auth.oauth is enabled.
	oauthBasePath := "/api/v1/oauth"
	externalURL := fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port)
	if cfg.Auth.OAuth {
		oauthSrv, err := oauth.New(&cfg.OAuth, oauthBasePath, externalURL)
		if err != nil {
			return nil, cleanup, fmt.Errorf("oauth: %w", err)
		}
		// Wire metrics so oauthTokenRefreshTotal increments on refresh.
		oauthSrv.SetMetrics(metrics)
		// Wire the persistent provider-token store + envelope-encryption
		// encryptor. The DSN defaults to ":memory:" when unset so
		// tests still work; production must set oauth.token_store_dsn.
		dsn := cfg.OAuth.TokenStoreDSN
		if dsn == "" {
			dsn = ":memory:"
		}
		enc, err := crypto.New(crypto.Config{
			Provider: cfg.OAuth.Crypto.Provider,
			Local: crypto.LocalConfig{
				MasterKeyPath: cfg.OAuth.Crypto.Local.MasterKeyPath,
				Passphrase:    cfg.OAuth.Crypto.Local.Passphrase,
			},
			Vault: crypto.VaultConfig{
				Address:   cfg.OAuth.Crypto.Vault.Address,
				TokenPath: cfg.OAuth.Crypto.Vault.TokenPath,
				KeyPath:   cfg.OAuth.Crypto.Vault.KeyPath,
			},
			Volcengine: crypto.VolcengineConfig{
				Region:    cfg.OAuth.Crypto.Volcengine.Region,
				AccessKey: cfg.OAuth.Crypto.Volcengine.AccessKey,
				SecretKey: cfg.OAuth.Crypto.Volcengine.SecretKey,
				KmsKeyID:  cfg.OAuth.Crypto.Volcengine.KmsKeyID,
			},
		})
		if err != nil {
			return nil, cleanup, fmt.Errorf("oauth crypto: %w", err)
		}
		tokenStore, err := oauth.NewStore(oauth.StoreConfig{
			DSN:       dsn,
			Encryptor: enc,
			Metrics:   metrics,
		})
		if err != nil {
			return nil, cleanup, fmt.Errorf("oauth token store: %w", err)
		}
		oauthSrv.SetTokenStore(tokenStore)
		prevCleanup := cleanup
		cleanup = func() {
			prevCleanup()
			_ = tokenStore.Close()
		}
		deps.OAuth = oauthSrv
	}

	// MCP server with the 13 real tools. Services are injected via
	// options so each tool can reach a real backend; nil services
	// degrade to "service not configured" error results.
	tempUpload, err := NewTempUploadStore(cfg.Server.TempUpload)
	if err != nil {
		return nil, cleanup, fmt.Errorf("temp upload store: %w", err)
	}
	inputGuard := NewLocalInputGuard(cfg.Server.LocalInputGuard)
	mcpSrv := mcp.New(
		mcp.WithRAGFS(mnt),
		mcp.WithVectorDB(vdb),
		mcp.WithSessions(sessionsStore),
		mcp.WithRetrieve(retriever),
		mcp.WithTempUpload(tempUpload),
		mcp.WithInputGuard(inputGuard),
	)
	deps.MCP = mcpSrv

	// Bot gateway. Constructed with no channels by default; channels
	// are registered by callers (cmd/openviking-server) via
	// gateway.RegisterChannel. When cfg.Bot.Enabled is false the
	// gateway is left nil so /bot/v1/* returns 501 UNSUPPORTED
	// (mirrors the OAuth wiring). When enabled, the gateway still
	// responds to per-channel routes with 404 for unregistered
	// channels — Health and ListChannels always return 200.
	if cfg.Bot.Enabled {
		deps.Bot = bot.NewGateway()
	}

	// Web-studio static assets. Served from cfg.Server.StudioPath when
	// configured; /studio/* returns 501 UNSUPPORTED when not. Embedding
	// is intentionally avoided so deployments can ship a single binary
	// plus a shared web-studio directory without rebuilding the server.
	//
	// OPENVIKING_WEB_STUDIO_DIR env overrides the config path so operators
	// can point at a built web-studio/dist without editing the config
	// file (mirrors the Python gate in openviking/server/app.py:659).
	studioPath := cfg.Server.StudioPath
	if env := os.Getenv("OPENVIKING_WEB_STUDIO_DIR"); env != "" {
		studioPath = env
	}
	if studioPath != "" {
		deps.StudioFS = http.Dir(studioPath)
	}

	// API key manager: argon2id-hashed JSON store at the configured
	// path. Constructed unconditionally so /admin/accounts/:id/api-keys
	// is always available; the file is created on first write. Errors
	// loading the file (corrupt JSON, etc.) are fatal so the operator
	// can fix the file before serving traffic.
	apiKeysPath := cfg.Server.APIKeysPath
	if apiKeysPath == "" {
		apiKeysPath = "./data/apikeys.json"
	}
	apiKeysMgr, err := apikeys.NewManager(apiKeysPath)
	if err != nil {
		return nil, cleanup, fmt.Errorf("apikeys: load %s: %w", apiKeysPath, err)
	}
	deps.APIKeys = apiKeysMgr
	engine.GET("/studio/*any", studioHandler(deps.StudioFS))

	// /web-access entry point — operator-facing URL convention that
	// redirects to /studio/ when the studio is configured. Mirrors the
	// rootRedirectHandler gate: only registered when StudioFS != nil so
	// /web-access falls through to NoRoute when the studio is absent.
	if deps.StudioFS != nil {
		engine.GET("/web-access", webAccessRedirectHandler())
	}

	// Favicon routes — always registered so /favicon.* and /mcp/favicon.*
	// never 404, even when web-studio isn't bundled. Files are embedded via
	// static.go. Mirrors openviking/server/app.py:633.
	registerFaviconRoutes(engine)

	// GET / → /studio/ convenience redirect, registered only when the
	// studio is configured (matches the Python gate at app.py:665).
	if deps.StudioFS != nil {
		engine.GET("/", rootRedirectHandler())
	}

	// 24 routers + WebDAV + bot + health/version/NoRoute.
	registerRoutes(engine, deps)

	// RFC 8414 / RFC 9728 .well-known endpoints live at the engine root.
	routers.RegisterWellKnown(engine, deps)

	// /openapi.json — live OpenAPI 3.0 spec enumerated from the gin route
	// tree. Web-studio's `pnpm gen-server-client` reads this endpoint to
	// regenerate its SDK so the SDK paths stay in sync with the Go server
	// without a hand-maintained spec file.
	registerOpenAPI(engine)

	// MCP streamable HTTP transport at /mcp.
	engine.Any("/mcp", gin.WrapH(mcpSrv.HTTPHandler()))

	// Metrics + pprof (no auth; protected by deployment firewall).
	engine.GET("/metrics", gin.WrapH(promhttp.Handler()))

	app := &App{
		cfg:    cfg,
		engine: engine,
		deps:   deps,
		queue:  queue,
	}

	return app, cleanup, nil
}

// Router returns the HTTP handler serving all routes.
func (a *App) Router() http.Handler { return a.engine }

// Deps returns the service container; tests and main.go can use it to
// inject real implementations as later phases land.
func (a *App) Deps() *routers.Deps { return a.deps }

// AsynqServer returns the queue runtime driving background ingestion.
func (a *App) AsynqServer() QueueServer { return a.queue }

// EnablePprof registers net/http/pprof handlers under /debug/pprof.
func (a *App) EnablePprof() {
	grp := a.engine.Group("/debug/pprof")
	grp.GET("/", gin.WrapF(pprof.Index))
	grp.GET("/cmdline", gin.WrapF(pprof.Cmdline))
	grp.GET("/profile", gin.WrapF(pprof.Profile))
	grp.GET("/symbol", gin.WrapF(pprof.Symbol))
	grp.GET("/trace", gin.WrapF(pprof.Trace))
	grp.GET("/:name", gin.WrapH(pprof.Handler("")))
}

// (no package-level queue handle: cleanup captures the local queue
// variable via closure so the handle is unnecessary.)

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"version": version.Version,
	})
}

func versionHandler(c *gin.Context) {
	c.JSON(http.StatusOK, version.Get())
}

// studioHandler serves web-studio static assets from the configured
// http.FileSystem. When fs is nil (no StudioPath configured) the handler
// returns 501 UNSUPPORTED so the server still boots and the studio is
// simply unavailable rather than crashing on every request. When the
// requested path maps to a real file on disk, the file is served
// directly. When the path does not map to a file (e.g. /studio/home,
// /studio/sessions), the handler falls back to serving index.html so
// the SPA's client-side router can resolve the route — mirrors the
// Python send_static_with_spa_fallback gate in openviking/server/app.py.
func studioHandler(fs http.FileSystem) gin.HandlerFunc {
	if fs == nil {
		return func(c *gin.Context) {
			abortWithError(c, domain.ErrUnsupported)
		}
	}
	fileServer := http.FileServer(fs)
	return func(c *gin.Context) {
		original := c.Request.URL.Path
		rel := strings.TrimPrefix(original, "/studio")
		if rel == "" {
			rel = "/"
		}
		// Real files (assets, favicon, etc.) are served via FileServer.
		if f, err := fs.Open(rel); err == nil {
			stat, _ := f.Stat()
			_ = f.Close()
			if stat != nil && !stat.IsDir() {
				c.Request.URL.Path = rel
				fileServer.ServeHTTP(c.Writer, c.Request)
				c.Request.URL.Path = original
				return
			}
		}
		// SPA fallback: serve index.html directly to avoid the
		// /index.html → / redirect loop that http.FileServer enforces.
		// Asset-like paths (with a file extension) MUST 404 when missing —
		// otherwise the browser would try to evaluate index.html as JS/CSS.
		if path.Ext(rel) != "" {
			http.NotFound(c.Writer, c.Request)
			return
		}
		f, err := fs.Open("/index.html")
		if err != nil {
			http.NotFound(c.Writer, c.Request)
			return
		}
		defer f.Close()
		stat, _ := f.Stat()
		if stat == nil {
			http.NotFound(c.Writer, c.Request)
			return
		}
		http.ServeContent(c.Writer, c.Request, stat.Name(), stat.ModTime(), f)
	}
}

// newVLM selects a VLM provider from cfg. Empty / unconfigured providers
// fall back to vlm.NewStub so the server still boots without network
// access (tests MUST NOT make network calls).
// newVLM selects a VLM provider from cfg. Empty / unconfigured providers
// return nil so VLMIntentAnalyzer.Analyze falls into its nil-safe path
// (returns the original query, no rewrite) and search degrades to
// sparse-only keyword matching. The stub is NOT used as the production
// default because its Chat returns ErrUnsupported, which would surface
// as a 500 from /api/v1/search on every fresh install.
func newVLM(cfg config.VLMConfig) vlm.VLM {
	switch cfg.Provider {
	case "openai":
		return vlm.NewOpenAI(cfg.APIBase, cfg.APIKey, cfg.Model, nil)
	case "codex":
		return vlm.NewCodex(cfg, nil)
	case "glm":
		return vlm.NewGLM(cfg, nil)
	case "kimi":
		return vlm.NewKimi(cfg, nil)
	case "litellm":
		return vlm.NewLiteLLM(cfg, nil)
	case "volcengine":
		return vlm.NewVolcengine(cfg, nil)
	case "dashscope":
		return vlm.NewDashscope(cfg, nil)
	default:
		return nil
	}
}

// newEmbedder selects an embedder provider from cfg. Empty / unconfigured
// providers fall back to embedder.NewLocal (hash stub) so the server still
// boots without network access.
func newEmbedder(cfg config.EmbedderConfig) embedder.Embedder {
	switch cfg.Provider {
	case "openai":
		return embedder.NewOpenAI(cfg.APIBase, cfg.APIKey, nil)
	case "volcengine":
		return embedder.NewVolcengine(cfg, nil)
	case "dashscope":
		return embedder.NewDashscope(cfg, nil)
	default:
		return embedder.NewLocal(cfg, nil)
	}
}

// newReranker selects a rerank provider from cfg. Empty / unconfigured
// providers fall back to rerank.NewLocal (cosine similarity) so the server
// still boots without network access.
func newReranker(cfg config.RerankConfig) rerank.Reranker {
	switch cfg.Provider {
	case "cohere":
		return rerank.NewCohere(cfg.APIBase, cfg.APIKey, cfg.Model, nil)
	case "volcengine":
		return rerank.NewVolcengine(cfg.APIBase, cfg.APIKey, cfg.Model, nil)
	case "dashscope":
		return rerank.NewDashscope(cfg.APIBase, cfg.APIKey, cfg.Model, nil)
	default:
		return rerank.NewLocal()
	}
}
