package routers

import (
	"bytes"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/queuefs"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterContent wires /api/v1/content/* — read/write resource bytes and
// layers. Layer reads use ?layer=abstract|overview|chunks(:n) on the same
// wildcard route to keep gin's radix tree happy.
//
// PUT triggers a queuefs DAG task so the indexing pipeline can pick up the
// new content (best-effort: when no queue is wired, the write still
// succeeds and the indexing happens lazily).
//
// SDK-compat aliases (Go SDK at sdk/go):
//   - POST /content/write  — body {uri,content,mode,wait}
//   - GET  /content/read?uri=...          (dispatched inside readContent)
//   - GET  /content/abstract?uri=...      (dispatched inside readContent)
//   - GET  /content/overview?uri=...      (dispatched inside readContent)
//   - POST /content/reindex — body {uri,mode,wait}
//
// The /read, /abstract, /overview aliases cannot be static routes because
// gin's radix tree doesn't allow GET /*uri and GET /read to coexist. They
// are dispatched inside readContent when the wildcard matches the alias
// path and a ?uri= query param is present.
func RegisterContent(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/content")
	r.GET("/*uri", readContent(deps))
	r.PUT("/*uri", writeContent(deps))
	r.POST("/write", writeContentByBody(deps))
	r.POST("/reindex", reindexContent(deps))
}

// contentPath resolves the request URI against the caller's account prefix.
func contentPath(c *gin.Context) string {
	uri := c.Param("uri")
	if uri == "" {
		uri = "/"
	}
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "content", uri))
	}
	return ragfs.Normalize(uri)
}

// readContent handles GET /content/*uri — read bytes (default) or a hidden
// sidecar layer (?layer=abstract|overview|chunks[:n]).
//
// SDK-compat dispatch: when the wildcard matches "/read", "/abstract",
// "/overview", or "/download" and a ?uri= query param is present, the
// request is treated as the Go SDK's GET /content/<alias>?uri=... call and
// dispatched to the matching handler. This dispatch is necessary because
// gin's radix tree doesn't allow GET /*uri and GET /read to coexist as
// separate routes. /download returns raw bytes with Content-Disposition;
// the other aliases return the okResponse envelope.
func readContent(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		// SDK-compat dispatch: GET /content/read?uri=...
		if c.Param("uri") == "/read" && c.Query("uri") != "" {
			readContentByQuery(deps)(c)
			return
		}
		// SDK-compat dispatch: GET /content/abstract?uri=...
		if c.Param("uri") == "/abstract" && c.Query("uri") != "" {
			readAbstractByQuery(deps)(c)
			return
		}
		// SDK-compat dispatch: GET /content/overview?uri=...
		if c.Param("uri") == "/overview" && c.Query("uri") != "" {
			readOverviewByQuery(deps)(c)
			return
		}
		// SDK-compat dispatch: GET /content/download?uri=... — returns blob.
		if c.Param("uri") == "/download" && c.Query("uri") != "" {
			downloadContentByQuery(deps)(c)
			return
		}
		p := contentPath(c)
		layer := c.Query("layer")
		switch {
		case layer == "" || layer == "raw":
			// Default: read raw bytes.
			var buf bytes.Buffer
			if err := deps.RAGFS.Read(c.Request.Context(), p, &buf); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			c.Data(http.StatusOK, "application/octet-stream", buf.Bytes())
		case layer == "abstract":
			s, err := ragfs.ReadAbstract(c.Request.Context(), deps.RAGFS, p)
			if err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			c.JSON(http.StatusOK, gin.H{"path": p, "layer": "abstract", "content": s})
		case layer == "overview":
			s, err := ragfs.ReadOverview(c.Request.Context(), deps.RAGFS, p)
			if err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			c.JSON(http.StatusOK, gin.H{"path": p, "layer": "overview", "content": s})
		case strings.HasPrefix(layer, "chunks"):
			// "chunks" -> list all; "chunks:3" -> read chunk index 3.
			names, err := ragfs.ListChunks(c.Request.Context(), deps.RAGFS, p)
			if err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			if idx := strings.TrimPrefix(layer, "chunks"); idx != "" {
				idx = strings.TrimPrefix(idx, ":")
				n, perr := strconv.Atoi(idx)
				if perr != nil || n < 0 || n >= len(names) {
					abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422,
						"chunk index out of range"))
					return
				}
				s, rerr := ragfs.ReadChunk(c.Request.Context(), deps.RAGFS, p, names[n])
				if rerr != nil {
					abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, rerr))
					return
				}
				c.JSON(http.StatusOK, gin.H{
					"path":        p,
					"layer":       "chunks",
					"chunk_index": n,
					"chunk_name":  names[n],
					"content":     s,
				})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"path":        p,
				"layer":       "chunks",
				"chunk_names": names,
			})
		default:
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422,
				"unsupported layer: "+layer))
		}
	}
}

// writeContent handles PUT /content/*uri — write bytes from the request body
// and best-effort enqueue an indexing task.
func writeContent(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := contentPath(c)
		if err := deps.RAGFS.Write(c.Request.Context(), p, c.Request.Body, 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		// Best-effort enqueue an indexing task. Failures do not fail the
		// write — the indexing pipeline catches up on the next scan.
		if deps.Queue != nil {
			id, ok := identity.FromContext(c.Request.Context())
			acct := ""
			if ok {
				acct = id.Account
			}
			_ = deps.Queue.Enqueue(&queuefs.Task{
				ID:       p,
				Type:     "parse",
				Payload:  []byte(p),
				Priority: queuefs.PriorityCritical,
			})
			_ = acct
		}
		c.JSON(http.StatusOK, gin.H{"path": p, "bytes_written": c.Request.ContentLength})
	}
}

// writeContentByBody handles POST /content/write — SDK-compat alias that
// accepts the URI in the JSON body ({"uri":..., "content":...}) instead of
// the path. Mirrors openviking/server/routers/content.py write_content so
// the Go SDK (sdk/go) Client.Write works without modification. The
// response uses the standard okResponse envelope so SDK's doJSON can
// unmarshal result into map[string]any.
func writeContentByBody(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req struct {
			URI     string `json:"uri"`
			Content string `json:"content"`
			Mode    string `json:"mode,omitempty"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.URI == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "uri is required"))
			return
		}
		p := resolveContentURI(c, req.URI)
		if err := deps.RAGFS.Write(c.Request.Context(), p, strings.NewReader(req.Content), 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if deps.Queue != nil {
			_ = deps.Queue.Enqueue(&queuefs.Task{
				ID:       p,
				Type:     "parse",
				Payload:  []byte(p),
				Priority: queuefs.PriorityCritical,
			})
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"path":         p,
			"bytes_written": int64(len(req.Content)),
			"mode":         req.Mode,
		}))
	}
}

// readContentByQuery handles GET /content/read?uri=... — SDK-compat alias
// that takes the URI as a query param instead of a path segment. Returns
// the standard okResponse envelope with result=<content string> so the
// Go SDK's Client.Read (which unmarshals result into *string) works.
func readContentByQuery(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		uri := c.Query("uri")
		if uri == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "uri is required"))
			return
		}
		p := resolveContentURI(c, uri)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), p, &buf); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(buf.String()))
	}
}

// resolveContentURI scopes a caller-supplied content URI to the caller's
// account prefix. Uses the SAME namespace as fsAbsPath (no "content"
// segment) so files written via POST /content/write are visible to
// GET /fs/stat?uri=... and GET /fs/ls. This matches the Python server's
// single-namespace behavior where content and fs routes share the same
// ragfs path tree.
//
// viking:// URIs are accepted (the "viking://" prefix is stripped) so SDK
// clients can pass viking://resources/... URIs to read/abstract/overview
// and get the same path resolution as fsAbsPath.
func resolveContentURI(c *gin.Context, uri string) string {
	if uri == "" {
		uri = "/"
	}
	if strings.HasPrefix(uri, "viking://") {
		uri = strings.TrimPrefix(uri, "viking://")
	}
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, uri))
	}
	return ragfs.Normalize(uri)
}

// readAbstractByQuery handles GET /content/abstract?uri=... — SDK-compat
// alias that returns the L0 abstract for a resource. The Go SDK unmarshals
// result into *string, so the response uses okResponse(string).
func readAbstractByQuery(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		uri := c.Query("uri")
		if uri == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "uri is required"))
			return
		}
		p := resolveContentURI(c, uri)
		s, err := ragfs.ReadAbstract(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(s))
	}
}

// readOverviewByQuery handles GET /content/overview?uri=... — SDK-compat
// alias that returns the L1 overview for a resource.
func readOverviewByQuery(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		uri := c.Query("uri")
		if uri == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "uri is required"))
			return
		}
		p := resolveContentURI(c, uri)
		s, err := ragfs.ReadOverview(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(s))
	}
}

// downloadContentByQuery handles GET /content/download?uri=... — returns
// the raw resource bytes with Content-Disposition: attachment so browsers
// save the file. Mirrors Python openviking/server/routers/content.py
// download_content. The filename is derived from the URI's last segment.
func downloadContentByQuery(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		uri := c.Query("uri")
		if uri == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "uri is required"))
			return
		}
		p := resolveContentURI(c, uri)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), p, &buf); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		filename := path.Base(p)
		if filename == "" || filename == "/" || filename == "." {
			filename = "download"
		}
		c.Header("Content-Type", "application/octet-stream")
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
		c.Data(http.StatusOK, "application/octet-stream", buf.Bytes())
	}
}

// reindexContent handles POST /content/reindex — SDK-compat alias that
// triggers reindexing for a URI. Best-effort: when a queuefs pipeline is
// wired, enqueues a parse task; otherwise returns ok with skipped=true.
// The Go SDK posts {uri, mode, wait}.
func reindexContent(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req struct {
			URI  string `json:"uri"`
			Mode string `json:"mode,omitempty"`
			Wait bool   `json:"wait,omitempty"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.URI == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "uri is required"))
			return
		}
		p := resolveContentURI(c, req.URI)
		mode := req.Mode
		if mode == "" {
			mode = "vectors_only"
		}
		skipped := true
		if deps.Queue != nil {
			_ = deps.Queue.Enqueue(&queuefs.Task{
				ID:       p,
				Type:     "parse",
				Payload:  []byte(p),
				Priority: queuefs.PriorityCritical,
			})
			skipped = false
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"path":    p,
			"mode":    mode,
			"wait":    req.Wait,
			"skipped": skipped,
		}))
	}
}
