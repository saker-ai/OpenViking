package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{Host: "127.0.0.1", Port: 0}}
	app, cleanup, err := BuildApp(cfg)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return app
}

func TestBuildAppMiddlewareChain(t *testing.T) {
	app := newTestApp(t)
	// /bot/v1/health returns 501 because deps.Bot is nil in the test
	// config; the identity middleware must let the request through (the
	// bot group sits outside /api/v1) and the middleware chain must stamp
	// the request ID and process time headers.
	req := httptest.NewRequest(http.MethodGet, "/bot/v1/health", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.NotEmpty(t, rec.Header().Get(HeaderRequestID))
	assert.NotEmpty(t, rec.Header().Get(HeaderProcessTime))
}

func TestBuildAppIdentityRejectsMissingAccount(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestBuildAppNoRouteStructuredError(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/no-such-path", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESOURCE_NOT_FOUND")
}

func TestBuildAppHealthAndVersion(t *testing.T) {
	app := newTestApp(t)
	for _, path := range []string{"/healthz", "/readyz", "/version", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		app.Router().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "path %s", path)
	}
}

func TestBuildAppSystemRouter(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/info", nil)
	req.Header.Set("X-OpenViking-Account", "acct")
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "go_version")
}

func TestErrorMiddlewareAppError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddleware())
	r.GET("/boom", func(c *gin.Context) {
		_ = c.Error(domain.ErrNotFound)
		c.Abort()
	})
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESOURCE_NOT_FOUND")
}

func TestErrorMiddlewareGenericError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddleware())
	r.GET("/boom", func(c *gin.Context) {
		_ = c.Error(domain.NewAppError("X", 500, http.StatusText(500)))
		c.Abort()
	})
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, 500, rec.Code)
}

func TestRegisterRoutesAllRouters(t *testing.T) {
	app := newTestApp(t)
	// Every router must be wired (not 404). Routes with real backends
	// return their natural response code; routes whose service dep is
	// nil in the test config (OAuth, Bot) return 501 UNSUPPORTED.
	// `implemented: true` means the route has a real handler (assert
	// non-404, non-501); `implemented: false` means the service is not
	// configured (assert 501).
	cases := []struct {
		method, path string
		implemented  bool
	}{
		{http.MethodGet, "/api/v1/admin/accounts", true},
		{http.MethodGet, "/api/v1/resources", true},
		{http.MethodGet, "/api/v1/fs/ls", true},
		{http.MethodGet, "/api/v1/content/foo", true},
		{http.MethodGet, "/api/v1/console/collections", true},
		{http.MethodPost, "/api/v1/search", true},
		{http.MethodPost, "/api/v1/code/outline", true},
		{http.MethodGet, "/api/v1/relations", true},
		{http.MethodGet, "/api/v1/sessions", true},
		{http.MethodGet, "/api/v1/skills", true},
		{http.MethodGet, "/api/v1/snapshot", true},
		{http.MethodGet, "/api/v1/stats/summary", true},
		{http.MethodPost, "/api/v1/pack/export", true},
		{http.MethodGet, "/api/v1/privacy-configs", true},
		{http.MethodGet, "/api/v1/debug/ctx", true},
		{http.MethodGet, "/api/v1/observer", true},
		{http.MethodGet, "/api/v1/tasks", true},
		{http.MethodGet, "/api/v1/user-settings", true},
		{http.MethodGet, "/api/v1/watches", true},
		{http.MethodPost, "/api/v1/oauth/register", false},
		{http.MethodOptions, "/webdav/resources/foo", true},
		{http.MethodGet, "/bot/v1/health", false},
		{http.MethodGet, "/studio/index.html", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("X-OpenViking-Account", "acct")
		rec := httptest.NewRecorder()
		app.Router().ServeHTTP(rec, req)
		if c.implemented {
			assert.NotEqualf(t, http.StatusNotFound, rec.Code, "path %s returned 404", c.path)
			assert.NotEqualf(t, http.StatusNotImplemented, rec.Code, "path %s returned 501", c.path)
		} else {
			assert.Equalf(t, http.StatusNotImplemented, rec.Code, "path %s returned %d", c.path, rec.Code)
		}
	}
}

// TestBuildAppStudioReturns501WhenNotConfigured verifies /studio/*
// returns 501 UNSUPPORTED when cfg.Server.StudioPath is empty (the
// default test config has no web-studio directory wired).
func TestBuildAppStudioReturns501WhenNotConfigured(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/studio/index.html", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Contains(t, rec.Body.String(), domain.CodeUnsupported)
}

// TestBuildAppStudioServesFilesWhenConfigured verifies /studio/* serves
// files from cfg.Server.StudioPath when set. Uses a temp dir with a
// single non-index.html asset to avoid http.FileServer's standard
// /index.html -> / redirect (covered by the next test).
func TestBuildAppStudioServesFilesWhenConfigured(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "app.js"), []byte("console.log(1)"), 0o644))

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:       "127.0.0.1",
			Port:       0,
			StudioPath: tmp,
		},
	}
	app, cleanup, err := BuildApp(cfg)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	req := httptest.NewRequest(http.MethodGet, "/studio/app.js", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "console.log(1)")
}

// TestBuildAppStudioIndexServedAtRoot verifies /studio/ (trailing slash)
// serves the directory index (index.html) when StudioPath is configured.
func TestBuildAppStudioIndexServedAtRoot(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "index.html"), []byte("root index"), 0o644))

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:       "127.0.0.1",
			Port:       0,
			StudioPath: tmp,
		},
	}
	app, cleanup, err := BuildApp(cfg)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	req := httptest.NewRequest(http.MethodGet, "/studio/", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "root index")
}

// TestBuildAppWebAccessRedirect verifies GET /web-access redirects to
// /studio/ (302) when StudioFS is configured. The route is only
// registered when StudioPath is set; otherwise /web-access falls
// through to NoRoute (404).
func TestBuildAppWebAccessRedirect(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "index.html"), []byte("root index"), 0o644))

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:       "127.0.0.1",
			Port:       0,
			StudioPath: tmp,
		},
	}
	app, cleanup, err := BuildApp(cfg)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	req := httptest.NewRequest(http.MethodGet, "/web-access", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/studio/", rec.Header().Get("Location"))
}

// TestBuildAppWebAccessNotRegisteredWhenStudioAbsent verifies /web-access
// returns 404 (NoRoute) when StudioPath is empty — the route is gated on
// StudioFS != nil, mirroring rootRedirectHandler.
func TestBuildAppWebAccessNotRegisteredWhenStudioAbsent(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/web-access", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestBuildAppFaviconRoutes verifies /favicon.ico, /favicon.png,
// /apple-touch-icon.png, and the /mcp/favicon.* siblings all return 200
// with the right Content-Type and a 24h Cache-Control header. Routes are
// always registered, even without a configured studio.
func TestBuildAppFaviconRoutes(t *testing.T) {
	app := newTestApp(t)
	cases := []struct {
		path        string
		contentType string
	}{
		{"/favicon.ico", "image/x-icon"},
		{"/favicon.png", "image/png"},
		{"/apple-touch-icon.png", "image/png"},
		{"/mcp/favicon.ico", "image/x-icon"},
		{"/mcp/favicon.png", "image/png"},
		{"/mcp/apple-touch-icon.png", "image/png"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		rec := httptest.NewRecorder()
		app.Router().ServeHTTP(rec, req)
		assert.Equalf(t, http.StatusOK, rec.Code, "path %s", c.path)
		assert.Equalf(t, c.contentType, rec.Header().Get("Content-Type"), "path %s", c.path)
		assert.Equalf(t, "public, max-age=86400", rec.Header().Get("Cache-Control"), "path %s", c.path)
		assert.NotEmptyf(t, rec.Body.Bytes(), "path %s body must be non-empty", c.path)
	}
}

// TestBuildAppFaviconMissingFileReturns500 verifies a malformed embed path
// surfaces as 500. This is a regression guard — embed.FS is compiled in,
// so the only way to reach this branch is a build regression that drops
// the file from static/. We can't trigger it at runtime, so the test
// documents the contract via makeFaviconHandler directly.
func TestBuildAppFaviconMissingFileReturns500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	sub, err := fs.Sub(staticFS, "static")
	require.NoError(t, err)
	r.GET("/missing", makeFaviconHandler(sub, faviconEntry{
		reqPath:   "/missing",
		fileName:  "does-not-exist.ico",
		mediaType: "image/x-icon",
	}))
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestBuildAppRootRedirectsToStudioWhenConfigured verifies GET / returns
// 302 to /studio/ when StudioPath is set. Mirrors the Python gate at
// app.py:665.
func TestBuildAppRootRedirectsToStudioWhenConfigured(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "index.html"), []byte("hi"), 0o644))
	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0, StudioPath: tmp},
	}
	app, cleanup, err := BuildApp(cfg)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/studio/", rec.Header().Get("Location"))
}

// TestBuildAppRootReturns404WhenStudioNotConfigured verifies / falls
// through to NoRoute (404 RESOURCE_NOT_FOUND) when StudioPath is empty.
func TestBuildAppRootReturns404WhenStudioNotConfigured(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESOURCE_NOT_FOUND")
}
