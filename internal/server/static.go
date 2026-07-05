package server

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed static/*
var staticFS embed.FS

// faviconEntry pairs a request path with the embedded file name and the
// media type to send in the Content-Type header.
type faviconEntry struct {
	reqPath   string
	fileName  string
	mediaType string
}

// faviconRoutes mirrors the Python routes in openviking/server/app.py:633.
// They are always registered so /favicon.* and /mcp/favicon.* never 404,
// even when web-studio isn't bundled. Source files live in static/ and are
// embedded so a single binary is enough to serve them. fileName is relative
// to the static/ sub-filesystem (see fs.Sub below).
var faviconRoutes = []faviconEntry{
	{"/favicon.ico", "favicon.ico", "image/x-icon"},
	{"/favicon.png", "favicon-32.png", "image/png"},
	{"/apple-touch-icon.png", "apple-touch-icon.png", "image/png"},
	{"/mcp/favicon.ico", "favicon.ico", "image/x-icon"},
	{"/mcp/favicon.png", "favicon-32.png", "image/png"},
	{"/mcp/apple-touch-icon.png", "apple-touch-icon.png", "image/png"},
}

// registerFaviconRoutes wires /favicon.* and /mcp/favicon.* routes onto the
// engine. Each route serves the embedded file with a 24-hour cache header,
// matching the Python implementation in app.py:632.
func registerFaviconRoutes(engine *gin.Engine) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed.FS is compiled in; Sub only fails on a malformed directive.
		// Treat as fatal so a build regression surfaces immediately.
		panic(err)
	}
	for _, e := range faviconRoutes {
		e := e
		engine.GET(e.reqPath, makeFaviconHandler(sub, e))
	}
}

// makeFaviconHandler returns a gin handler that serves a single embedded
// file with caching headers. It is closed over the entry to avoid aliasing.
func makeFaviconHandler(fsys fs.FS, e faviconEntry) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := fs.ReadFile(fsys, e.fileName)
		if err != nil {
			c.String(http.StatusInternalServerError, "favicon %q: %v", e.fileName, err)
			return
		}
		c.Header("Content-Type", e.mediaType)
		c.Header("Cache-Control", "public, max-age=86400")
		_, _ = c.Writer.Write(body)
	}
}

// rootRedirectHandler redirects GET / to /studio/ so users hitting the bare
// origin land on the Web Studio UI when it is configured. When the studio is
// not configured the route is left unregistered so / falls through to
// NoRoute (404 RESOURCE_NOT_FOUND), matching the Python gate in app.py:665.
func rootRedirectHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/studio/")
	}
}

// webAccessRedirectHandler redirects GET /web-access to /studio/ so the
// "web-access" entry point (a common operator-facing URL convention) lands
// on the Web Studio UI without requiring a separate static-asset tree. The
// route is only registered when StudioFS is configured; otherwise /web-access
// falls through to NoRoute, mirroring rootRedirectHandler's gate.
func webAccessRedirectHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/studio/")
	}
}
