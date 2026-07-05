package routers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/privacy"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/localfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/s3fs"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// RegisterConsole wires /api/v1/console/* — direct vectordb / collection
// console plus ragfs mount management.
//
// The collection endpoints delegate to vectordb.CollectionAdapter. Mount
// endpoints delegate to ragfs.MountableFS when the configured RAGFS is a
// mountable. Health pings every mounted backend.
//
// SDK BFF endpoints (Python openviking/server/routers/console.py): the
// dashboard / tokens / context-commits / audit endpoints return
// {enabled:false,message:...} when usage-audit is not configured so the
// Studio UI degrades gracefully instead of 404.
func RegisterConsole(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/console")
	r.GET("/collections", consoleListCollections(deps))
	r.POST("/collections", consoleCreateCollection(deps))
	r.DELETE("/collections/:name", consoleDropCollection(deps))
	r.GET("/health", consoleHealth(deps))
	r.POST("/collections/:name/upsert", consoleUpsert(deps))
	r.POST("/collections/:name/search", consoleSearch(deps))
	r.POST("/collections/:name/delete", consoleDelete(deps))
	// SDK BFF endpoints consumed by web-studio home + request-logs pages.
	r.GET("/dashboard/summary", consoleDashboardSummary(deps))
	r.GET("/tokens", consoleTokens(deps))
	r.GET("/context-commits", consoleContextCommits(deps))
	r.GET("/audit", consoleAudit(deps))
}

// disabledUsageAudit is the standard payload returned by console BFF
// endpoints when no usage-audit store is wired. Mirrors Python's
// OvMaybeDisabled model — Studio renders the "disabled" banner instead of
// crashing on a 404.
func disabledUsageAudit() gin.H {
	return gin.H{
		"enabled": false,
		"message": "usage audit not configured; set ov.usage_audit.enabled=true",
	}
}

// consoleDashboardSummary handles GET /console/dashboard/summary — today's
// retrieval / token / context counts. Returns OvMaybeDisabled when usage
// audit is not wired.
func consoleDashboardSummary(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, okResponse(gin.H{
			"enabled":  false,
			"message":  "usage audit not configured",
			"context_counts": gin.H{
				"total":   0,
				"files":   0,
				"memories": 0,
				"skills":  0,
			},
			"today_retrievals": gin.H{
				"total":  0,
				"search": 0,
				"find":   0,
			},
			"today_tokens": gin.H{
				"total":            0,
				"vlm_input":        0,
				"vlm_output":       0,
				"embedding_input":  0,
			},
		}))
	}
}

// consoleTokens handles GET /console/tokens — daily token series for the
// requested date range. Requires start_date and end_date query params.
func consoleTokens(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := c.Query("start_date")
		end := c.Query("end_date")
		if start == "" || end == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422,
				"start_date and end_date are required (YYYY-MM-DD)"))
			return
		}
		items := buildEmptyTokenSeries(start, end)
		c.JSON(http.StatusOK, okResponse(gin.H{
			"enabled":    false,
			"message":    "usage audit not configured",
			"start_date": start,
			"end_date":   end,
			"bucket":     defaultIfEmpty(c.Query("bucket"), "day"),
			"items":      items,
		}))
	}
}

// consoleContextCommits handles GET /console/context-commits — hourly /
// 4-hour bucketed context commit counts for the requested date range.
func consoleContextCommits(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := c.Query("start_date")
		end := c.Query("end_date")
		if start == "" || end == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422,
				"start_date and end_date are required (YYYY-MM-DD)"))
			return
		}
		items := buildEmptyContextCommitSeries(start, end, defaultIfEmpty(c.Query("bucket"), "hour"))
		c.JSON(http.StatusOK, okResponse(gin.H{
			"enabled":    false,
			"message":    "usage audit not configured",
			"start_date": start,
			"end_date":   end,
			"bucket":     defaultIfEmpty(c.Query("bucket"), "hour"),
			"items":      items,
		}))
	}
}

// consoleAudit handles GET /console/audit — paginated audit log entries.
// Returns an empty page with OvMaybeDisabled when usage audit is not
// configured so Studio's request-logs page renders the disabled state.
func consoleAudit(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, _ := strconv.Atoi(c.Query("page"))
		if page <= 0 {
			page = 1
		}
		pageSize, _ := strconv.Atoi(c.Query("page_size"))
		if pageSize <= 0 {
			pageSize = 10
		}
		if pageSize > 100 {
			pageSize = 100
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"enabled":     false,
			"message":     "usage audit not configured",
			"items":       []any{},
			"page":        page,
			"page_size":   pageSize,
			"total":       0,
			"success_rate": 0,
		}))
	}
}

// defaultIfEmpty returns v when non-empty, else fallback.
func defaultIfEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// buildEmptyTokenSeries returns one zero-valued item per day in [start, end].
// Studio's token-trend-panel expects a point per day to render the chart.
func buildEmptyTokenSeries(start, end string) []gin.H {
	out := []gin.H{}
	startT, err1 := time.Parse("2006-01-02", start)
	endT, err2 := time.Parse("2006-01-02", end)
	if err1 != nil || err2 != nil || endT.Before(startT) {
		return out
	}
	for d := startT; !d.After(endT); d = d.AddDate(0, 0, 1) {
		out = append(out, gin.H{
			"date":             d.Format("2006-01-02"),
			"total":            0,
			"vlm_input":        0,
			"vlm_output":       0,
			"embedding_input":  0,
		})
	}
	return out
}

// buildEmptyContextCommitSeries returns one zero-valued item per bucket in
// [start, end]. Bucket is "hour" or "4h"; default is hourly. Each item
// carries date + hour fields so the heatmap renders.
func buildEmptyContextCommitSeries(start, end, bucket string) []gin.H {
	out := []gin.H{}
	startT, err1 := time.Parse("2006-01-02", start)
	endT, err2 := time.Parse("2006-01-02", end)
	if err1 != nil || err2 != nil || endT.Before(startT) {
		return out
	}
	step := time.Hour
	if bucket == "4h" {
		step = 4 * time.Hour
	}
	// End-of-day for endT so the range is inclusive of the last day.
	for d := startT; !d.After(endT.AddDate(0, 0, 1)); d = d.Add(step) {
		out = append(out, gin.H{
			"date":                  d.Format("2006-01-02"),
			"hour":                  d.Hour(),
			"total":                 0,
			"session_commit":        0,
			"session_add_message":   0,
			"add_resource":          0,
			"add_skill":             0,
		})
	}
	return out
}

// mountable returns the *ragfs.MountableFS when deps.RAGFS is one. The
// console mount endpoints are only available when the underlying RAGFS
// supports mount management.
func mountable(deps *Deps) *ragfs.MountableFS {
	if deps == nil || deps.RAGFS == nil {
		return nil
	}
	if m, ok := deps.RAGFS.(*ragfs.MountableFS); ok {
		return m
	}
	return nil
}

// consoleListCollections handles GET /console/collections — list mounted
// ragfs backends plus vectordb collections when configured.
func consoleListCollections(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		out := gin.H{}
		if m := mountable(deps); m != nil {
			backends := m.Backends()
			names := make([]string, 0, len(backends))
			for k := range backends {
				names = append(names, k)
			}
			out["mounts"] = names
		}
		if deps != nil && deps.VectorDB != nil {
			colls, err := deps.VectorDB.ListCollections(c.Request.Context())
			if err != nil {
				abortWithError(c, domain.Wrap(domain.CodeVectorDBError, 500, err))
				return
			}
			out["collections"] = colls
		}
		c.JSON(http.StatusOK, out)
	}
}

// createCollectionRequest is the JSON body for POST /console/collections.
// It serves two roles depending on the "kind" field:
//   - kind="mount": mount a ragfs backend (admin-only; returns 403 when
//     no admin token is configured — see Constraints in the task spec).
//   - kind="collection": create a vectordb collection via EnsureCollection.
type createCollectionRequest struct {
	Kind     string                    `json:"kind"` // "mount" | "collection"
	Name     string                    `json:"name"`
	Backend  string                    `json:"backend"`            // for mount: local|memory|s3
	Settings map[string]any            `json:"settings,omitempty"` // for mount: backend-specific
	Schema   vectordb.CollectionSchema `json:"schema,omitempty"`   // for collection

	// Local backend parameters. Path is the OS directory rooted at the
	// mount; the directory is created if missing.
	Path string `json:"path,omitempty"`

	// S3 backend parameters. Endpoint and Bucket are required; the rest
	// default to empty strings (anonymous or env-derived credentials).
	Endpoint  string `json:"endpoint,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Region    string `json:"region,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// consoleCreateCollection handles POST /console/collections — mount a
// backend (admin-only) or create a vectordb collection.
func consoleCreateCollection(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req createCollectionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		switch req.Kind {
		case "", "collection":
			if deps.VectorDB == nil {
				_ = c.Error(domain.ErrUnsupported)
				c.Abort()
				return
			}
			schema := req.Schema
			if schema.Name == "" {
				schema.Name = req.Name
			}
			if schema.Name == "" {
				abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "name is required"))
				return
			}
			if err := deps.VectorDB.EnsureCollection(c.Request.Context(), schema); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeVectorDBError, 500, err))
				return
			}
			c.JSON(http.StatusCreated, gin.H{"collection": schema.Name})
		case "mount":
			m := mountable(deps)
			if m == nil {
				abortWithError(c, domain.NewAppError(domain.CodeUnsupported, 501,
					"ragfs is not mountable"))
				return
			}
			// Admin-only: the task spec allows returning 403 when not
			// configured. We gate on the presence of an admin header;
			// a real admin token check lands in the auth phase.
			if c.GetHeader("X-OpenViking-Admin-Token") == "" {
				abortWithError(c, domain.ErrForbidden)
				return
			}
			prefix := req.Name
			if prefix == "" {
				abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "name is required"))
				return
			}
			if !strings.HasPrefix(prefix, "/") {
				prefix = "/" + prefix
			}
			backend, backendName, appErr := newMountBackend(&req)
			if appErr != nil {
				abortWithError(c, appErr)
				return
			}
			if err := m.Mount(prefix, backend); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			c.JSON(http.StatusCreated, gin.H{"mount": prefix, "backend": backendName})
		default:
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422,
				"unknown kind: "+req.Kind))
		}
	}
}

// newMountBackend constructs a ragfs.FileSystem for the console mount request
// according to req.Backend. It validates backend-specific fields (path for
// local, endpoint+bucket for s3) and returns a structured *domain.AppError so
// the caller can pass it straight to abortWithError.
//
// Supported backends: "" / "memory" (memfs), "local" (localfs), "s3" (s3fs).
// The s3fs constructor returns a usable stub when no S3 client is wired; real
// network I/O lands in a later phase, but the mount table still registers the
// backend so console routes can list/unmount it.
func newMountBackend(req *createCollectionRequest) (ragfs.FileSystem, string, *domain.AppError) {
	switch req.Backend {
	case "", "memory":
		fs, err := newMemoryBackend(req.Name)
		if err != nil {
			return nil, "", domain.Wrap(domain.CodeRAGFSError, 500, err)
		}
		return fs, "memory", nil
	case "local":
		if strings.TrimSpace(req.Path) == "" {
			return nil, "", domain.NewAppError(domain.CodeValidationFailed, 422,
				"path is required for local backend")
		}
		fs, err := localfs.New(req.Name, req.Path)
		if err != nil {
			return nil, "", domain.Wrap(domain.CodeRAGFSError, 500, err)
		}
		return fs, "local", nil
	case "s3":
		if strings.TrimSpace(req.Endpoint) == "" || strings.TrimSpace(req.Bucket) == "" {
			return nil, "", domain.NewAppError(domain.CodeValidationFailed, 422,
				"endpoint and bucket are required for s3 backend")
		}
		fs := s3fs.New(req.Name, req.Endpoint, req.Bucket, req.Region,
			req.AccessKey, req.SecretKey, req.Prefix, req.ReadOnly)
		return fs, "s3", nil
	default:
		return nil, "", domain.NewAppError(domain.CodeValidationFailed, 422,
			"unknown backend: "+req.Backend)
	}
}

// consoleDropCollection handles DELETE /console/collections/:name — unmount
// a ragfs backend (when name starts with "/") or drop a vectordb collection.
func consoleDropCollection(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		name := c.Param("name")
		if name == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "name is required"))
			return
		}
		// ragfs paths arrive URL-encoded with a leading slash; gin decodes
		// them. Treat any name containing "/" as a mount path.
		if strings.Contains(name, "/") {
			m := mountable(deps)
			if m == nil {
				abortWithError(c, domain.NewAppError(domain.CodeUnsupported, 501,
					"ragfs is not mountable"))
				return
			}
			if !strings.HasPrefix(name, "/") {
				name = "/" + name
			}
			if err := m.Unmount(name); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			c.JSON(http.StatusOK, gin.H{"unmounted": name})
			return
		}
		if deps.VectorDB == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		if err := deps.VectorDB.DropCollection(c.Request.Context(), name); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeVectorDBError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"dropped": name})
	}
}

// consoleHealth handles GET /console/health — health-check every mounted
// ragfs backend plus the vectordb adapter.
func consoleHealth(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		out := gin.H{"status": "ok"}
		if m := mountable(deps); m != nil {
			if err := m.HealthCheck(c.Request.Context()); err != nil {
				out["status"] = "degraded"
				out["ragfs_error"] = err.Error()
			}
		}
		if deps != nil && deps.VectorDB != nil {
			colls, err := deps.VectorDB.ListCollections(c.Request.Context())
			if err != nil {
				out["status"] = "degraded"
				out["vectordb_error"] = err.Error()
			} else {
				out["collections"] = len(colls)
			}
		}
		c.JSON(http.StatusOK, out)
	}
}

// upsertRequest is the JSON body for POST /console/collections/:name/upsert.
// Vectors carries the rows to insert or replace by ID.
type upsertRequest struct {
	Vectors []vectordb.Vector `json:"vectors"`
}

// consoleUpsert handles POST /console/collections/:name/upsert — insert or
// replace rows by ID. The collection must already exist (created via POST
// /console/collections with kind=collection).
func consoleUpsert(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.VectorDB == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		name := c.Param("name")
		if name == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "name is required"))
			return
		}
		var req upsertRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if len(req.Vectors) == 0 {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "vectors is required"))
			return
		}
		// Redact PII from text-bearing metadata fields before the
		// vectors reach the vectordb backend. The redacted form is
		// what's stored and retrieved; per the privacy pipeline
		// design, retrieval does NOT restore (callers see redacted
		// text). Only known text-bearing keys are redacted so filter
		// fields like "account" / "kind" pass through unchanged.
		for i := range req.Vectors {
			redactVectorMetadata(&req.Vectors[i])
		}
		if err := deps.VectorDB.Upsert(c.Request.Context(), name, req.Vectors); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeVectorDBError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"collection": name, "upserted": len(req.Vectors)})
	}
}

// textMetadataKeys lists vectordb Vector.Metadata keys that typically
// hold prose text and may carry PII. Conservative: only well-known
// text keys are redacted; custom keys are passed through unchanged.
var textMetadataKeys = []string{"text", "content", "description", "summary"}

// redactVectorMetadata redacts PII in the known text-bearing metadata
// fields of a vectordb Vector in place. Per-request PIIMaps are
// discarded (the redacted form is what's stored; retrieval does not
// restore).
func redactVectorMetadata(v *vectordb.Vector) {
	if v == nil || v.Metadata == nil {
		return
	}
	for _, key := range textMetadataKeys {
		s, ok := v.Metadata[key].(string)
		if !ok || s == "" {
			continue
		}
		redacted, _ := privacy.Redact(s)
		v.Metadata[key] = redacted
	}
}

// consoleSearchRequest is the JSON body for POST /console/collections/:name/search.
type consoleSearchRequest struct {
	Query  []float32      `json:"query"`
	TopK   int            `json:"top_k"`
	Filter vectordb.Filter `json:"filter,omitempty"`
}

// consoleSearch handles POST /console/collections/:name/search — return the
// TopK rows nearest to Query (filtered by Filter). Hits are ordered best-first.
func consoleSearch(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.VectorDB == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		name := c.Param("name")
		if name == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "name is required"))
			return
		}
		var req consoleSearchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if len(req.Query) == 0 {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "query is required"))
			return
		}
		if req.TopK <= 0 {
			req.TopK = 10
		}
		params := vectordb.SearchParams{
			Collection: name,
			Query:      req.Query,
			TopK:       req.TopK,
			Filter:     req.Filter,
		}
		result, err := deps.VectorDB.Search(c.Request.Context(), params)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeVectorDBError, 500, err))
			return
		}
		hits := []vectordb.Vector{}
		if result != nil && len(result.Hits) > 0 {
			hits = result.Hits
		}
		c.JSON(http.StatusOK, gin.H{"collection": name, "hits": hits})
	}
}

// deleteRequest is the JSON body for POST /console/collections/:name/delete.
// IDs lists the rows to remove; unknown IDs are ignored by the adapter.
type deleteRequest struct {
	IDs []string `json:"ids"`
}

// consoleDelete handles POST /console/collections/:name/delete — remove the
// rows with the given IDs.
func consoleDelete(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.VectorDB == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		name := c.Param("name")
		if name == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "name is required"))
			return
		}
		var req deleteRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if len(req.IDs) == 0 {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "ids is required"))
			return
		}
		if err := deps.VectorDB.Delete(c.Request.Context(), name, req.IDs); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeVectorDBError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"collection": name, "deleted": len(req.IDs)})
	}
}
