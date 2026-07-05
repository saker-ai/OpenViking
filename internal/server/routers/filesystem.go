package routers

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterFilesystem wires /api/v1/fs/* — filesystem-paradigm ops
// (ls, stat, tree, mkdir, move, copy, grep, rm).
//
// All paths are scoped to the caller's account when identity is present:
// /accounts/{account}/fs/<path>. When identity is absent the raw path is
// used (tests only).
//
// SDK-compat aliases:
//   - POST /fs/mv           → fsMove (SDK sends {from_uri,to_uri})
//   - DELETE /fs            → fsRemove (SDK sends ?uri=...&recursive=...)
//   - POST /fs/attrs/set_tags → fsSetTags (SDK sends {uri,tags,mode,recursive})
//
// The /move and /rm routes are kept for the Go server's own clients; the
// SDK aliases live alongside them. DELETE /fs doesn't conflict with the
// POST routes (different methods).
func RegisterFilesystem(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/fs")
	r.GET("/ls", fsList(deps))
	r.GET("/stat", fsStat(deps))
	r.GET("/attrs", fsStat(deps)) // SDK-compat alias: Go SDK calls /fs/attrs.
	r.GET("/attrs/set_tags", fsSetTagsGet(deps)) // for completeness; SDK uses POST
	r.GET("/tree", fsTree(deps))
	r.GET("/grep", fsGrep(deps))
	r.POST("/mkdir", fsMkdir(deps))
	r.POST("/move", fsMove(deps))
	r.POST("/mv", fsMove(deps))       // SDK-compat alias
	r.POST("/copy", fsCopy(deps))
	r.POST("/rm", fsRemove(deps))
	r.DELETE("", fsRemove(deps))      // SDK-compat alias: DELETE /fs?uri=...
	r.POST("/attrs/set_tags", fsSetTags(deps))
}

// fsRequest is the common JSON body for fs endpoints that take a path.
// Move and Copy additionally take a "to" field. The URI/FromURI/ToURI
// fields are SDK-compat aliases — the Go SDK posts {uri:...} for single-path
// ops and {from_uri,to_uri} for move/copy.
type fsRequest struct {
	Path        string   `json:"path"`
	URI         string   `json:"uri,omitempty"`
	FromURI     string   `json:"from_uri,omitempty"`
	To          string   `json:"to,omitempty"`
	ToURI       string   `json:"to_uri,omitempty"`
	Recursive   bool     `json:"recursive,omitempty"`
	Mode        uint32   `json:"mode,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Depth       int      `json:"depth,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	TagMode     string   `json:"mode,omitempty"` // for SetTags: "replace"|"append"
}

// pathField returns the request's primary path, preferring the SDK's "uri"
// alias when set. For move/copy, FromURI is preferred over Path.
func (req *fsRequest) pathField() string {
	if req.URI != "" {
		return req.URI
	}
	if req.FromURI != "" {
		return req.FromURI
	}
	return req.Path
}

// toField returns the request's destination path, preferring the SDK's
// "to_uri" alias when set.
func (req *fsRequest) toField() string {
	if req.ToURI != "" {
		return req.ToURI
	}
	return req.To
}

// fsAbsPath resolves a path string against the caller's account prefix.
// The "fs" segment is dropped from the resulting ragfs path so that
// /accounts/{account}/fs/docs -> /accounts/{account}/docs.
//
// viking:// URIs are accepted: the "viking://" scheme prefix is stripped
// and the remainder (e.g. "resources/docs", "memories/abc") is joined
// under the caller's account. This aligns with the Go SDK's NormalizeURI
// convention so SDK clients can pass viking:// URIs to ls/stat/mv/rm/etc.
func fsAbsPath(c *gin.Context, p string) string {
	if p == "" {
		p = "/"
	}
	if strings.HasPrefix(p, "viking://") {
		p = strings.TrimPrefix(p, "viking://")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, p))
	}
	return ragfs.Normalize(p)
}

// fsQueryPath returns the effective path query parameter for an fs endpoint.
// The Python sibling openviking/server/routers/filesystem.py uses "uri" as
// the query parameter name; the Go implementation has historically used
// "path". We accept both, with "uri" taking precedence so Python clients
// get 1:1 shape parity.
func fsQueryPath(c *gin.Context, goAlias string) string {
	if v := c.Query("uri"); v != "" {
		return v
	}
	return c.Query(goAlias)
}

// fsList handles GET /fs/ls?path=...&uri=...
//
// The Python sibling openviking/server/routers/filesystem.py ls takes a
// required "uri" query parameter. We accept both "uri" (Python field, takes
// precedence) and "path" (Go alias) so either client shape works.
//
// Response shape mirrors Python: {"status":"ok","result":[<entry>,...]}.
// The result is the entries array directly (not wrapped under "entries")
// so the Go SDK's List — which unmarshals result into []any — works.
// When the path does not exist yet, an empty list is returned rather than
// an error so callers can bootstrap.
func fsList(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := fsAbsPath(c, fsQueryPath(c, "path"))
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, okResponse([]any{}))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if entries == nil {
			entries = []*ragfs.TreeEntry{}
		}
		c.JSON(http.StatusOK, okResponse(entries))
	}
}

// fsStat handles GET /fs/stat?path=...&uri=...
//
// The Python sibling openviking/server/routers/filesystem.py stat takes a
// required "uri" query parameter. We accept both "uri" (Python field, takes
// precedence) and "path" (Go alias).
//
// Response shape mirrors Python: {"status":"ok","result":{...}}. The
// result includes the ragfs FileInfo under "info" and any tags set via
// POST /fs/attrs/set_tags under "tags" (read from the .tags.json hidden
// sidecar so SDK Attrs surfaces tags without a separate read).
func fsStat(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := fsAbsPath(c, fsQueryPath(c, "path"))
		info, err := deps.RAGFS.Stat(c.Request.Context(), p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		tags := []string{}
		if existing, rerr := ragfs.ReadHidden(c.Request.Context(), deps.RAGFS, p, ".tags.json"); rerr == nil {
			_ = json.Unmarshal([]byte(existing), &tags)
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"path": p,
			"info": info,
			"tags": tags,
		}))
	}
}

// fsTree handles GET /fs/tree?path=...&depth=N
func fsTree(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := fsAbsPath(c, c.Query("path"))
		depth := 1
		if d := c.Query("depth"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 {
				depth = n
			}
		}
		entries, err := deps.RAGFS.TreeDirectory(c.Request.Context(), p, depth)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p, "depth": depth, "entries": entries})
	}
}

// fsGrep handles GET /fs/grep?pattern=...&path=...&recursive=1
func fsGrep(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		pattern := c.Query("pattern")
		if pattern == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "pattern is required"))
			return
		}
		p := fsAbsPath(c, c.Query("path"))
		recursive := c.Query("recursive") == "1"
		matches, err := deps.RAGFS.Grep(c.Request.Context(), pattern, p, recursive)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p, "pattern": pattern, "matches": matches})
	}
}

// fsMkdir handles POST /fs/mkdir
func fsMkdir(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req fsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.pathField() == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "path is required"))
			return
		}
		mode := os.FileMode(req.Mode)
		if mode == 0 {
			mode = 0o755
		}
		p := fsAbsPath(c, req.pathField())
		if err := deps.RAGFS.Mkdir(c.Request.Context(), p, mode); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{"path": p, "description": req.Description})
	}
}

// fsMove handles POST /fs/move — rename within the same mount (cross-mount
// forbidden by ragfs.MountableFS.Rename).
func fsMove(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req fsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.pathField() == "" || req.toField() == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "path and to are required"))
			return
		}
		src := fsAbsPath(c, req.pathField())
		dst := fsAbsPath(c, req.toField())
		if err := deps.RAGFS.Rename(c.Request.Context(), src, dst); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"from": src, "to": dst})
	}
}

// fsCopy handles POST /fs/copy — copy within the same mount.
func fsCopy(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req fsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.pathField() == "" || req.toField() == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "path and to are required"))
			return
		}
		src := fsAbsPath(c, req.pathField())
		dst := fsAbsPath(c, req.toField())
		if err := deps.RAGFS.Copy(c.Request.Context(), src, dst); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{"from": src, "to": dst})
	}
}

// fsRemove handles POST /fs/rm and DELETE /fs — remove a path (recursive
// when set). The DELETE /fs alias reads the URI from the ?uri= query param
// (SDK convention) when the body is empty; POST /fs/rm reads from the body.
func fsRemove(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req fsRequest
		if c.Request.Body != nil && c.Request.ContentLength != 0 {
			if err := c.ShouldBindJSON(&req); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
				return
			}
		}
		// DELETE /fs?uri=... takes precedence over the body path field.
		path := req.pathField()
		if q := c.Query("uri"); q != "" {
			path = q
		}
		if path == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "path is required"))
			return
		}
		recursive := req.Recursive || c.Query("recursive") == "1" || c.Query("recursive") == "true"
		p := fsAbsPath(c, path)
		if err := deps.RAGFS.Remove(c.Request.Context(), p, recursive); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p, "deleted": true})
	}
}

// fsSetTags handles POST /fs/attrs/set_tags — SDK-compat endpoint that
// stores k=v retrieval tags as a hidden sidecar (.tags.json) next to the
// resource. Mode "replace" (default) overwrites; "append" merges. The
// hidden file is read back by GET /fs/attrs so the SDK's Attrs method
// surfaces tags without a separate read.
func fsSetTags(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req fsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.URI == "" && req.Path == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "uri is required"))
			return
		}
		p := fsAbsPath(c, req.pathField())
		tags := req.Tags
		if tags == nil {
			tags = []string{}
		}
		mode := req.TagMode
		if mode == "" {
			mode = "replace"
		}
		// Read existing tags when appending.
		merged := tags
		if mode == "append" {
			if existing, err := ragfs.ReadHidden(c.Request.Context(), deps.RAGFS, p, ".tags.json"); err == nil {
				var prev []string
				if jsonErr := json.Unmarshal([]byte(existing), &prev); jsonErr == nil {
					seen := make(map[string]struct{}, len(prev))
					for _, t := range prev {
						seen[t] = struct{}{}
					}
					for _, t := range tags {
						if _, ok := seen[t]; !ok {
							prev = append(prev, t)
							seen[t] = struct{}{}
						}
					}
					merged = prev
				}
			}
		}
		body, err := json.Marshal(merged)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		if err := ragfs.WriteHidden(c.Request.Context(), deps.RAGFS, p, ".tags.json", string(body)); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"path": p,
			"tags": merged,
			"mode": mode,
		}))
	}
}

// fsSetTagsGet handles GET /fs/attrs/set_tags — returns the current tags
// for a URI. Not used by the SDK (which POSTs), but provided for symmetry
// and so callers can read tags back without parsing the hidden file.
func fsSetTagsGet(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		uri := c.Query("uri")
		if uri == "" {
			uri = c.Query("path")
		}
		if uri == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "uri is required"))
			return
		}
		p := fsAbsPath(c, uri)
		tags := []string{}
		if existing, err := ragfs.ReadHidden(c.Request.Context(), deps.RAGFS, p, ".tags.json"); err == nil {
			_ = json.Unmarshal([]byte(existing), &tags)
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"path": p, "tags": tags}))
	}
}
