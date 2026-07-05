package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterRelations wires /api/v1/relations/* — relation graph.
//
// Edges are stored as a single JSON document at
// /accounts/{account}/relations/.meta.json so the graph survives process
// restarts and is portable across ragfs backends. The document is small
// (a list of edges) so a full read-modify-write per request is acceptable;
// callers needing higher throughput should batch via the queue.
func RegisterRelations(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/relations")
	r.GET("", listRelations(deps))
	r.POST("", createRelation(deps))
	r.DELETE("/*id", deleteRelation(deps))
	r.GET("/neighbors", relationNeighbors(deps))
	r.GET("/paths", relationPaths(deps))
}

// relationEdge is one directed edge in the relation graph.
type relationEdge struct {
	ID       string         `json:"id"`
	Source   string         `json:"source"`
	Target   string         `json:"target"`
	Kind     string         `json:"kind,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// relationGraph is the persisted document.
type relationGraph struct {
	Edges []relationEdge `json:"edges"`
}

// createRelationRequest is the JSON body for POST /relations.
type createRelationRequest struct {
	Source   string         `json:"source"`
	Target   string         `json:"target"`
	Kind     string         `json:"kind,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// relationsMetaPath returns the ragfs path to the relations metadata file
// for the caller's account. When identity is absent a global path is used
// (tests only).
func relationsMetaPath(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "relations", ".meta.json"))
	}
	return "/relations/.meta.json"
}

// loadRelations reads the graph document. A missing file is treated as an
// empty graph so a fresh account returns [] instead of an error.
func loadRelations(ctx context.Context, fs ragfs.FileSystem, p string) (*relationGraph, error) {
	var buf bytes.Buffer
	if err := fs.Read(ctx, p, &buf); err != nil {
		if ragfs.IsNotFound(err) {
			return &relationGraph{Edges: []relationEdge{}}, nil
		}
		return nil, err
	}
	var g relationGraph
	if err := json.Unmarshal(buf.Bytes(), &g); err != nil {
		return nil, err
	}
	if g.Edges == nil {
		g.Edges = []relationEdge{}
	}
	return &g, nil
}

// saveRelations writes the graph document atomically. ragfs.Write is
// overwrite-in-place for memfs and most backends; callers needing atomicity
// across backends should layer a temp-rename, which is out of scope here.
func saveRelations(ctx context.Context, fs ragfs.FileSystem, p string, g *relationGraph) error {
	if err := fs.Mkdir(ctx, path.Dir(p), 0o755); err != nil {
		// ignore "already exists" so creating the parent each call is cheap.
		_ = err
	}
	body, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return fs.Write(ctx, p, bytes.NewReader(body), 0o644)
}

// listRelations handles GET /relations — list all edges in the caller's graph.
func listRelations(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := relationsMetaPath(c)
		g, err := loadRelations(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"edges": g.Edges, "path": p})
	}
}

// createRelation handles POST /relations — add an edge to the graph.
// The edge ID is generated server-side so callers can reference it for DELETE.
func createRelation(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req createRelationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if strings.TrimSpace(req.Source) == "" || strings.TrimSpace(req.Target) == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "source and target are required"))
			return
		}
		p := relationsMetaPath(c)
		g, err := loadRelations(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		edge := relationEdge{
			ID:       newEdgeID(g),
			Source:   req.Source,
			Target:   req.Target,
			Kind:     req.Kind,
			Metadata: req.Metadata,
		}
		g.Edges = append(g.Edges, edge)
		if err := saveRelations(c.Request.Context(), deps.RAGFS, p, g); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{"edge": edge, "path": p})
	}
}

// deleteRelation handles DELETE /relations/:id — remove an edge by ID.
func deleteRelation(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := strings.TrimPrefix(c.Param("id"), "/")
		if id == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "id is required"))
			return
		}
		p := relationsMetaPath(c)
		g, err := loadRelations(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		removed := false
		filtered := g.Edges[:0]
		for _, e := range g.Edges {
			if e.ID == id {
				removed = true
				continue
			}
			filtered = append(filtered, e)
		}
		if !removed {
			abortWithError(c, domain.NewAppError(domain.CodeResourceNotFound, 404, "relation not found"))
			return
		}
		g.Edges = filtered
		if err := saveRelations(c.Request.Context(), deps.RAGFS, p, g); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "deleted": true})
	}
}

// relationNeighbors handles GET /relations/neighbors?node=<uri> — return
// all edges that touch the given node (as source or target).
func relationNeighbors(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		node := strings.TrimSpace(c.Query("node"))
		if node == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "node is required"))
			return
		}
		p := relationsMetaPath(c)
		g, err := loadRelations(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		neighbors := make([]relationEdge, 0)
		for _, e := range g.Edges {
			if e.Source == node || e.Target == node {
				neighbors = append(neighbors, e)
			}
		}
		c.JSON(http.StatusOK, gin.H{"node": node, "neighbors": neighbors})
	}
}

// relationPaths handles GET /relations/paths?source=<uri>&target=<uri> —
// find all simple paths from source to target via BFS (max depth 6 to bound
// the search).
func relationPaths(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		source := strings.TrimSpace(c.Query("source"))
		target := strings.TrimSpace(c.Query("target"))
		if source == "" || target == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "source and target are required"))
			return
		}
		p := relationsMetaPath(c)
		g, err := loadRelations(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		paths := findPaths(g, source, target, 6)
		c.JSON(http.StatusOK, gin.H{"source": source, "target": target, "paths": paths})
	}
}

// findPaths returns all simple paths from source to target up to maxDepth
// edges long. The graph is small (a single account's relations doc) so BFS
// with a visited set is fine.
func findPaths(g *relationGraph, source, target string, maxDepth int) [][]string {
	adj := make(map[string][]string)
	for _, e := range g.Edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
	}
	var paths [][]string
	var walk func(node string, visited map[string]bool, acc []string)
	walk = func(node string, visited map[string]bool, acc []string) {
		if node == target {
			cp := make([]string, len(acc)+1)
			copy(cp, acc)
			cp[len(acc)] = node
			paths = append(paths, cp)
			return
		}
		if len(acc) >= maxDepth {
			return
		}
		visited[node] = true
		acc = append(acc, node)
		for _, next := range adj[node] {
			if visited[next] {
				continue
			}
			walk(next, visited, acc)
		}
		visited[node] = false
	}
	walk(source, make(map[string]bool), nil)
	return paths
}

// newEdgeID generates a short, unique-within-graph edge ID. The counter
// approach keeps IDs small and human-readable; collisions are prevented by
// scanning existing IDs for the next free index.
func newEdgeID(g *relationGraph) string {
	used := make(map[string]bool, len(g.Edges))
	for _, e := range g.Edges {
		used[e.ID] = true
	}
	for i := 1; i <= len(g.Edges)+1; i++ {
		id := fmt.Sprintf("e%d", i)
		if !used[id] {
			return id
		}
	}
	return fmt.Sprintf("e%d", len(g.Edges)+1)
}
