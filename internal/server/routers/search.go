package routers

import (
	"context"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/retrieve"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterSearch wires /api/v1/search/* — hybrid retrieval entrypoint.
//
// Three endpoints:
//   - POST /search          — run the hierarchical retriever, return ranked docs
//   - POST /search/explain  — same call but surface intent + stats for debugging
//   - GET  /search/suggestions?q=... — prefix-match path suggestions from ragfs
//
// The retriever interface only exposes Retrieve; explain re-uses it and
// decorates the response with an "explain" flag so callers can request the
// intent/stats payload without a separate code path.
func RegisterSearch(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/search")
	r.POST("", searchQuery(deps))
	r.POST("/explain", searchExplain(deps))
	r.GET("/suggestions", searchSuggestions(deps))
	// SDK-compat aliases. /find and /search share the searchQuery payload
	// shape ({query:...}) so they alias directly. /grep and /glob take a
	// different payload ({uri,pattern} and {pattern,uri}) so they have
	// dedicated handlers (searchGrep / searchGlob) that call ragfs.Grep
	// and a ReadDir+filepath.Match glob walk respectively.
	r.POST("/find", searchQuery(deps))
	r.POST("/search", searchQuery(deps))
	r.POST("/grep", searchGrep(deps))
	r.POST("/glob", searchGlob(deps))
}

// searchRequest is the JSON body for POST /search and /search/explain.
// It maps 1:1 onto retrieve.RetrieveRequest; the account is filled from
// the request identity so callers can't spoof another tenant.
type searchRequest struct {
	Query     string         `json:"query"`
	TopK      int            `json:"top_k,omitempty"`
	Level     retrieve.Level `json:"level,omitempty"`
	URIPrefix string         `json:"uri_prefix,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Filter    map[string]any `json:"filter,omitempty"` // alias for Metadata
}

// toRetrieveRequest builds a RetrieveRequest scoped to the caller's account.
// TopK defaults to 10 when unset so a bare {"query":"..."} body still works.
func (req *searchRequest) toRetrieveRequest(account string) retrieve.RetrieveRequest {
	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}
	meta := req.Metadata
	if meta == nil && req.Filter != nil {
		meta = req.Filter
	}
	prefix := req.URIPrefix
	if prefix != "" {
		if scoped, ok := normalizeAccountPrefix(account, prefix); ok {
			prefix = scoped
		}
	}
	return retrieve.RetrieveRequest{
		Account:   account,
		Query:     req.Query,
		TopK:      topK,
		Level:     req.Level,
		URIPrefix: prefix,
		Metadata:  meta,
	}
}

// normalizeAccountPrefix scopes a caller-supplied URI prefix to the caller's
// account root when it isn't already scoped. Returns false when the prefix
// is empty (caller keeps the empty value).
func normalizeAccountPrefix(account, prefix string) (string, bool) {
	if prefix == "" {
		return "", false
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if strings.HasPrefix(prefix, "/accounts/"+account+"/") || prefix == "/accounts/"+account {
		return ragfs.Normalize(prefix), true
	}
	return ragfs.Normalize(path.Join("/accounts", account, prefix)), true
}

// searchQuery handles POST /search — run the retriever, return ranked docs.
func searchQuery(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Retrieve == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req searchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if strings.TrimSpace(req.Query) == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "query is required"))
			return
		}
		account := accountFromContext(c)
		resp, err := deps.Retrieve.Retrieve(c.Request.Context(), req.toRetrieveRequest(account))
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": resp})
	}
}

// searchExplain handles POST /search/explain — same as /search but returns
// the intent + stats payload prominently so callers can inspect the funnel.
func searchExplain(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Retrieve == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req searchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if strings.TrimSpace(req.Query) == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "query is required"))
			return
		}
		account := accountFromContext(c)
		resp, err := deps.Retrieve.Retrieve(c.Request.Context(), req.toRetrieveRequest(account))
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"explain": true,
			"result":  resp,
			"intent":  resp.Intent,
			"stats":   resp.Stats,
		})
	}
}

// searchSuggestions handles GET /search/suggestions?q=... — return path
// suggestions from the caller's resources root. The match is a simple
// case-insensitive prefix on the basename so the endpoint works without a
// search index. When q is empty the first 50 entries under the root are
// returned as a directory listing.
func searchSuggestions(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		q := strings.ToLower(strings.TrimSpace(c.Query("q")))
		root := accountResourcesRoot(c)
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), root)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, gin.H{"suggestions": []string{}, "query": q})
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		limit := 50
		suggestions := make([]string, 0, limit)
		for _, e := range entries {
			if e == nil || e.Info == nil {
				continue
			}
			name := strings.ToLower(e.Info.Name)
			if q != "" && !strings.HasPrefix(name, q) {
				continue
			}
			suggestions = append(suggestions, e.Info.Name)
			if len(suggestions) >= limit {
				break
			}
		}
		c.JSON(http.StatusOK, gin.H{"suggestions": suggestions, "query": q})
	}
}

// accountFromContext returns the caller's account from the request identity,
// or "anonymous" when identity is absent (tests only).
func accountFromContext(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return id.Account
	}
	return "anonymous"
}

// searchGrep handles POST /search/grep — SDK-compat endpoint that runs
// ragfs.Grep against the caller's account-scoped URI prefix. The Go SDK
// posts {"uri":..., "pattern":..., "case_insensitive":...} and expects
// result to be a map with the match list. Mirrors the Python sibling's
// grep endpoint shape.
func searchGrep(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req struct {
			URI             string `json:"uri,omitempty"`
			Pattern         string `json:"pattern,omitempty"`
			CaseInsensitive bool   `json:"case_insensitive,omitempty"`
			ExcludeURI      string `json:"exclude_uri,omitempty"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.Pattern == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "pattern is required"))
			return
		}
		p := req.URI
		if p == "" {
			p = "/"
		}
		// Strip the viking:// scheme prefix so NormalizeURI("/") ("viking://")
		// resolves to the account root instead of being mangled by path.Join
		// into "viking:". Matches fsAbsPath / resolveContentURI behavior.
		if strings.HasPrefix(p, "viking://") {
			p = strings.TrimPrefix(p, "viking://")
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
			p = ragfs.Normalize(path.Join("/accounts", id.Account, p))
		} else {
			p = ragfs.Normalize(p)
		}
		pattern := req.Pattern
		if req.CaseInsensitive {
			pattern = "(?i)" + pattern
		}
		matches, err := deps.RAGFS.Grep(c.Request.Context(), pattern, p, true)
		if err != nil {
			// A not-found on the search root is treated as empty matches
			// (mirrors fsList's behavior) so callers can grep a path that
			// hasn't been populated yet without surfacing a 500.
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, okResponse(gin.H{
					"matches": []any{},
					"pattern": req.Pattern,
					"uri":     req.URI,
					"count":   0,
				}))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"matches":  matches,
			"pattern":  req.Pattern,
			"uri":      req.URI,
			"count":    len(matches),
		}))
	}
}

// searchGlob handles POST /search/glob — SDK-compat endpoint that walks
// the caller's account-scoped URI tree and returns paths matching a
// filepath.Match glob pattern. The Go SDK posts {"pattern":..., "uri":...}
// and expects result to be a map with the match list.
func searchGlob(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req struct {
			Pattern string `json:"pattern,omitempty"`
			URI     string `json:"uri,omitempty"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.Pattern == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "pattern is required"))
			return
		}
		p := req.URI
		if p == "" {
			p = "/"
		}
		// Strip the viking:// scheme prefix so NormalizeURI("/") ("viking://")
		// resolves to the account root. Matches fsAbsPath / resolveContentURI
		// / searchGrep behavior.
		if strings.HasPrefix(p, "viking://") {
			p = strings.TrimPrefix(p, "viking://")
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
			p = ragfs.Normalize(path.Join("/accounts", id.Account, p))
		} else {
			p = ragfs.Normalize(p)
		}
		matches := make([]string, 0, 16)
		walkGlob(c.Request.Context(), deps.RAGFS, p, req.Pattern, &matches, 1000)
		c.JSON(http.StatusOK, okResponse(gin.H{
			"matches": matches,
			"pattern": req.Pattern,
			"uri":     req.URI,
			"count":   len(matches),
		}))
	}
}

// walkGlob recursively walks the ragfs tree from root, appending paths
// whose base name matches pattern. Stops after limit matches to bound
// work on large trees. Errors are tolerated (a missing subtree is skipped).
func walkGlob(ctx context.Context, fs ragfs.FileSystem, root, pattern string, out *[]string, limit int) {
	if len(*out) >= limit {
		return
	}
	entries, err := fs.ReadDir(ctx, root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e == nil || e.Info == nil {
			continue
		}
		name := e.Info.Name
		full := ragfs.Normalize(path.Join(root, name))
		if matched, _ := filepath.Match(pattern, name); matched {
			*out = append(*out, full)
			if len(*out) >= limit {
				return
			}
		}
		if e.Info.IsDir {
			walkGlob(ctx, fs, full, pattern, out, limit)
		}
	}
}

// accountResourcesRoot returns the ragfs path for the caller's resources root.
// When identity is absent the bare "/" is returned (tests only).
func accountResourcesRoot(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "resources"))
	}
	return "/"
}
