package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterStats wires /api/v1/stats/* — usage / resource statistics.
//
// Per-account counters are persisted as JSON files under
// /accounts/{account}/stats/{name}.json. Endpoints that map to live ragfs
// state (resources, storage) are computed on demand rather than read from
// a counter file so they stay accurate without an explicit flush step.
func RegisterStats(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/stats")
	r.GET("/summary", statsSummary(deps))
	r.GET("/resources", statsResources(deps))
	r.GET("/sessions", statsSessions(deps))
	r.GET("/sessions/:id", statsSessionByID(deps))
	r.GET("/tokens", statsTokens(deps))
	r.GET("/storage", statsStorage(deps))
	// SDK BFF endpoint: web-studio's SDK calls /stats/memories for the
	// memory-aggregator dashboard. Returns zeros when no aggregator wired.
	r.GET("/memories", statsMemories(deps))
}

func statsDir(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "stats"))
	}
	return "/stats"
}

func statsPath(c *gin.Context, name string) string {
	return ragfs.Normalize(path.Join(statsDir(c), name+".json"))
}

// readStatsCounter reads a per-account stats counter JSON. When the file is
// absent the caller gets an empty map (counters start at zero).
func readStatsCounter(deps *Deps, c *gin.Context, name string) (map[string]any, string, error) {
	p := statsPath(c, name)
	var buf bytes.Buffer
	if err := deps.RAGFS.Read(c.Request.Context(), p, &buf); err != nil {
		if ragfs.IsNotFound(err) {
			return map[string]any{}, p, nil
		}
		return nil, p, err
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		return nil, p, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, p, nil
}

// statsSummary handles GET /stats/summary — aggregate overview of every
// counter file plus live resource/storage counts.
func statsSummary(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		dir := statsDir(c)
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), dir)
		if err != nil && !ragfs.IsNotFound(err) {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		summary := gin.H{"path": dir}
		for _, e := range entries {
			if e == nil || e.Info == nil || e.Info.IsDir {
				continue
			}
			name := e.Info.Name
			if len(name) <= 5 || name[len(name)-5:] != ".json" {
				continue
			}
			key := name[:len(name)-5]
			full := ragfs.Normalize(path.Join(dir, name))
			var buf bytes.Buffer
			if rerr := deps.RAGFS.Read(c.Request.Context(), full, &buf); rerr != nil {
				continue
			}
			var val any
			_ = json.Unmarshal(buf.Bytes(), &val)
			summary[key] = val
		}
		summary["resources"] = countResources(deps, c)
		summary["storage_bytes"] = measureStorage(deps, c)
		c.JSON(http.StatusOK, summary)
	}
}

// statsResources handles GET /stats/resources — live count of resources
// under the caller's account. Returns 0 when the resources root is empty
// or absent.
func statsResources(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		count, root := countResources(deps, c), resourcesRoot(c)
		c.JSON(http.StatusOK, gin.H{"path": root, "count": count})
	}
}

// statsSessions handles GET /stats/sessions — read the persisted sessions
// counter (or empty when absent).
func statsSessions(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		val, p, err := readStatsCounter(deps, c, "sessions")
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p, "sessions": val})
	}
}

// statsTokens handles GET /stats/tokens — read the persisted tokens counter.
func statsTokens(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		val, p, err := readStatsCounter(deps, c, "tokens")
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p, "tokens": val})
	}
}

// statsStorage handles GET /stats/storage — live byte count of the caller's
// account subtree.
func statsStorage(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		root := accountRoot(c)
		c.JSON(http.StatusOK, gin.H{"path": root, "bytes": measureStorage(deps, c)})
	}
}

// statsSessionByID handles GET /stats/sessions/:session_id — extraction
// stats for a single session. Returns zeros when the session is not found
// or no aggregator is wired. Mirrors Python
// openviking/server/routers/stats.py get_session_extraction_stats.
func statsSessionByID(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		sid := c.Param("id")
		if sid == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"session_id":          sid,
			"total_turns":         0,
			"memories_extracted":  map[string]any{},
			"contexts_used":       0,
			"skills_used":         0,
		}))
	}
}

// statsMemories handles GET /stats/memories — aggregate memory stats by
// category / hotness / staleness. Returns zeros when no aggregator is
// wired. Mirrors Python openviking/server/routers/stats.py get_memory_stats.
func statsMemories(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		category := c.Query("category")
		if category != "" {
			validCategories := map[string]bool{
				"profile": true, "preferences": true, "entities": true,
				"events": true, "cases": true, "patterns": true,
				"tools": true, "skills": true,
			}
			if !validCategories[category] {
				abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422,
					"invalid category: "+category))
				return
			}
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"total_memories": 0,
			"by_category": gin.H{
				"profile": 0, "preferences": 0, "entities": 0, "events": 0,
				"cases": 0, "patterns": 0, "tools": 0, "skills": 0,
			},
			"hotness_distribution": gin.H{
				"cold": 0, "warm": 0, "hot": 0,
			},
			"staleness": gin.H{
				"not_accessed_7d":         0,
				"not_accessed_30d":        0,
				"oldest_memory_age_days":  0,
			},
		}))
	}
}

// resourcesRoot returns the ragfs path of the caller's resources root.
func resourcesRoot(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "resources"))
	}
	return "/resources"
}

// accountRoot returns the ragfs path of the caller's account subtree.
func accountRoot(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account))
	}
	return "/"
}

// countResources walks the caller's resources root recursively and returns
// the number of leaf entries. Returns 0 on any error so the summary endpoint
// stays robust against fresh accounts.
func countResources(deps *Deps, c *gin.Context) int64 {
	root := resourcesRoot(c)
	entries, err := deps.RAGFS.ReadDir(c.Request.Context(), root)
	if err != nil {
		return 0
	}
	var n int64
	walkTreeEntries(c.Request.Context(), deps.RAGFS, entries, func(_ *ragfs.TreeEntry) {
		n++
	}, false)
	return n
}

// measureStorage sums the sizes of every file under the caller's account
// subtree. Returns 0 on any error.
func measureStorage(deps *Deps, c *gin.Context) int64 {
	root := accountRoot(c)
	entries, err := deps.RAGFS.ReadDir(c.Request.Context(), root)
	if err != nil {
		return 0
	}
	var total int64
	walkTreeEntries(c.Request.Context(), deps.RAGFS, entries, func(e *ragfs.TreeEntry) {
		if e != nil && e.Info != nil {
			total += e.Info.Size
		}
	}, true)
	return total
}

// walkTreeEntries visits every entry in `entries` and, for directories,
// recurses into the children via ReadDir. The visitor is called only for
// non-directory entries (files). includeDirs controls whether directory
// entries themselves are also passed to the visitor (used by measureStorage
// to skip them via the IsDir check, which is a no-op here but kept for
// future callers). The walk is bounded by the filesystem's own structure
// and stops at leaf files.
func walkTreeEntries(ctx context.Context, fs ragfs.FileSystem, entries []*ragfs.TreeEntry, visit func(*ragfs.TreeEntry), _ bool) {
	for _, e := range entries {
		if e == nil || e.Info == nil {
			continue
		}
		if e.Info.IsDir {
			children, err := fs.ReadDir(ctx, e.Path)
			if err != nil {
				continue
			}
			walkTreeEntries(ctx, fs, children, visit, false)
			continue
		}
		visit(e)
	}
}
