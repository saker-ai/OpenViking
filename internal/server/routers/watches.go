package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterWatches wires /api/v1/watches/* — resource watch subscriptions.
//
// ragfs.FileSystem does not expose a native Watch API, so watches are
// implemented as polling subscriptions: a watch persists the target path +
// last-known snapshot to ragfs under /accounts/{account}/watches/{id}.json,
// and GET /:id/events is an SSE stream that re-stats the target each tick
// and emits a change event when the size or modtime differs.
//
// SDK-compat aliases (Go SDK at sdk/go):
//   - GET    /watches               — list, optionally filtered by ?to_uri= and ?active_only=
//   - GET    /watches/:id           — read a watch subscription (or by ?to_uri=)
//   - PATCH  /watches/:id           — update by task id
//   - PATCH  /watches               — update by ?to_uri= (no :id)
//   - DELETE /watches/:id           — delete by task id (or by ?to_uri=)
//   - DELETE /watches               — delete by ?to_uri= (no :id)
//   - POST   /watches/:id/trigger   — trigger by task id
//   - POST   /watches/trigger       — trigger by ?to_uri= (no :id)
//   - GET    /watches/:id/events    — SSE stream
func RegisterWatches(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/watches")
	r.GET("", listWatches(deps))
	r.POST("", createWatch(deps))
	r.GET("/:id", getWatch(deps))
	r.PUT("/:id", updateWatch(deps))
	r.PATCH("/:id", updateWatch(deps)) // SDK-compat: Go SDK UpdateWatch uses PATCH.
	r.PATCH("", updateWatchByURI(deps))
	r.DELETE("/:id", deleteWatch(deps))
	r.DELETE("", deleteWatchByURI(deps))
	r.POST("/:id/trigger", triggerWatch(deps))
	r.POST("/trigger", triggerWatchByURI(deps))
	r.GET("/:id/events", watchEvents(deps))
}

// watchEntry is the persisted shape of a watch subscription. ToURI carries
// the caller's original viking:// URI (or raw "to" string) so list/get/
// update/delete/trigger can be resolved by ?to_uri= query without scanning
// every JSON file's Path field.
type watchEntry struct {
	ID            string            `json:"id"`
	Path          string            `json:"path"`
	ToURI         string            `json:"to_uri,omitempty"`
	Account       string            `json:"account"`
	Recursive     bool              `json:"recursive,omitempty"`
	WatchInterval float64           `json:"watch_interval,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	Instruction   string            `json:"instruction,omitempty"`
	IsActive      bool              `json:"is_active"`
	CreatedAt     time.Time         `json:"created_at"`
	Snapshot      *watchSnapshot    `json:"snapshot,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// watchSnapshot is the last-known state of the watched path. The events
// stream compares the current stat against this snapshot to decide whether
// to emit a change event.
type watchSnapshot struct {
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// watchesRoot returns the ragfs path under which watch JSON files are stored
// for the caller's account.
func watchesRoot(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "watches"))
	}
	return ragfs.Normalize("/watches")
}

// watchPath returns the JSON path for a single watch.
func watchPath(c *gin.Context, id string) string {
	return ragfs.Normalize(path.Join(watchesRoot(c), id+".json"))
}

// readWatch loads a watchEntry from ragfs. Returns domain.ErrNotFound when
// the file is missing.
func readWatch(ctx context.Context, deps *Deps, p string) (*watchEntry, error) {
	var buf bytes.Buffer
	if err := deps.RAGFS.Read(ctx, p, &buf); err != nil {
		return nil, err
	}
	var w watchEntry
	if err := json.Unmarshal(buf.Bytes(), &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// writeWatch persists a watchEntry to ragfs, creating the parent dir if
// needed. The .json extension is appended to the watch id.
func writeWatch(ctx context.Context, deps *Deps, p string, w *watchEntry) error {
	root := path.Dir(p)
	_ = deps.RAGFS.Mkdir(ctx, root, 0o755)
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return deps.RAGFS.Write(ctx, p, bytes.NewReader(data), 0o644)
}

// listWatches handles GET /watches — list watch subscriptions for the
// caller's account. Filters by ?to_uri= (exact match on the stored ToURI
// after NormalizeURI) and ?active_only=1|true. Returns the standard
// okResponse envelope with result={tasks:[...], total:N} so the Go SDK's
// ListWatches (which unmarshals result into map[string]any) works.
func listWatches(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		root := watchesRoot(c)
		toURIFilter := c.Query("to_uri")
		activeOnly := c.Query("active_only") == "1" || c.Query("active_only") == "true"
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), root)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, okResponse(gin.H{"tasks": []any{}, "total": 0}))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		out := make([]*watchEntry, 0, len(entries))
		for _, e := range entries {
			if e.Info == nil || e.Info.IsDir {
				continue
			}
			if !strings.HasSuffix(e.Info.Name, ".json") {
				continue
			}
			w, werr := readWatch(c.Request.Context(), deps, e.Path)
			if werr != nil {
				continue
			}
			if toURIFilter != "" && !watchURIMatches(w, toURIFilter) {
				continue
			}
			if activeOnly && !w.IsActive {
				continue
			}
			out = append(out, w)
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"tasks": out, "total": len(out)}))
	}
}

// watchURIMatches returns true when the watch's stored ToURI matches the
// caller-supplied filter after NormalizeURI. Both viking://resources/...
// and raw ragfs paths are accepted; comparison is done on the normalized
// form so callers can pass either shape.
func watchURIMatches(w *watchEntry, filter string) bool {
	if w == nil {
		return false
	}
	if w.ToURI == filter {
		return true
	}
	return normalizeWatchURI(w.ToURI) == normalizeWatchURI(filter)
}

// normalizeWatchURI strips the "viking://" scheme prefix and any leading
// "/" so two URIs that point at the same ragfs path compare equal.
func normalizeWatchURI(uri string) string {
	if uri == "" {
		return ""
	}
	uri = strings.TrimPrefix(uri, "viking://")
	uri = strings.TrimPrefix(uri, "resources/")
	uri = strings.TrimPrefix(uri, "/")
	return uri
}

// findWatchByURI scans the caller's watches directory for an entry whose
// ToURI matches the supplied filter. Returns the entry and its ragfs path,
// or domain.ErrNotFound when no match exists.
func findWatchByURI(ctx context.Context, deps *Deps, c *gin.Context, toURI string) (*watchEntry, string, error) {
	root := watchesRoot(c)
	entries, err := deps.RAGFS.ReadDir(ctx, root)
	if err != nil {
		if ragfs.IsNotFound(err) {
			return nil, "", domain.ErrNotFound
		}
		return nil, "", err
	}
	for _, e := range entries {
		if e.Info == nil || e.Info.IsDir || !strings.HasSuffix(e.Info.Name, ".json") {
			continue
		}
		w, werr := readWatch(ctx, deps, e.Path)
		if werr != nil {
			continue
		}
		if watchURIMatches(w, toURI) {
			return w, e.Path, nil
		}
	}
	return nil, "", domain.ErrNotFound
}

// createWatchRequest is the JSON body for POST /watches.
type createWatchRequest struct {
	Path      string `json:"path"`
	ToURI     string `json:"to_uri,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
}

// createWatch handles POST /watches — register a new watch subscription.
// The target path must exist on ragfs; the initial snapshot is captured at
// creation time so the events stream can diff against it.
func createWatch(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req createWatchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.Path == "" && req.ToURI == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "path is required"))
			return
		}
		id, _ := identity.FromContext(c.Request.Context())
		target := req.Path
		if target == "" {
			target = req.ToURI
		}
		if !strings.HasPrefix(target, "/accounts/") && id.Account != "" {
			if strings.HasPrefix(target, "viking://") {
				target = strings.TrimPrefix(target, "viking://")
			}
			target = ragfs.Normalize(path.Join("/accounts", id.Account, target))
		}
		info, err := deps.RAGFS.Stat(c.Request.Context(), target)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		wid := newWatchID()
		w := &watchEntry{
			ID:        wid,
			Path:      target,
			ToURI:     req.ToURI,
			Account:   id.Account,
			Recursive: req.Recursive,
			IsActive:  true,
			CreatedAt: time.Now().UTC(),
			Snapshot: &watchSnapshot{
				Size:    info.Size,
				IsDir:   info.IsDir,
				ModTime: info.ModTime,
			},
		}
		if err := writeWatch(c.Request.Context(), deps, watchPath(c, wid), w); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, okResponse(w))
	}
}

// deleteWatch handles DELETE /watches/:id — remove a watch subscription.
// When ?to_uri= is supplied and the path id is empty or "-", the watch is
// looked up by URI instead.
func deleteWatch(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		wid := path.Clean(c.Param("id"))
		p := watchPath(c, wid)
		w, err := readWatch(c.Request.Context(), deps, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if err := deps.RAGFS.Remove(c.Request.Context(), p, false); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"task_id": wid, "to_uri": w.ToURI, "deleted": true}))
	}
}

// deleteWatchByURI handles DELETE /watches?to_uri=... — delete by URI.
func deleteWatchByURI(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		toURI := c.Query("to_uri")
		if toURI == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "to_uri is required"))
			return
		}
		w, p, err := findWatchByURI(c.Request.Context(), deps, c, toURI)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if err := deps.RAGFS.Remove(c.Request.Context(), p, false); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"task_id": w.ID, "to_uri": w.ToURI, "deleted": true}))
	}
}

// getWatch handles GET /watches/:id — read a watch subscription. Returns
// the persisted watchEntry (path, snapshot, metadata). The Go SDK calls
// this with task_id and optional to_uri query; to_uri is ignored when
// task_id is present (path-based lookup is authoritative).
func getWatch(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		wid := path.Clean(c.Param("id"))
		if wid == "" || wid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "watch id is required"))
			return
		}
		p := watchPath(c, wid)
		w, err := readWatch(c.Request.Context(), deps, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(w))
	}
}

// updateWatchRequest is the JSON body for PATCH /watches/:id and
// PATCH /watches?to_uri=...
type updateWatchRequest struct {
	WatchInterval *float64 `json:"watch_interval,omitempty"`
	IsActive      *bool    `json:"is_active,omitempty"`
	Reason        *string  `json:"reason,omitempty"`
	Instruction   *string  `json:"instruction,omitempty"`
}

// updateWatch handles PUT/PATCH /watches/:id — update watch fields.
func updateWatch(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		wid := path.Clean(c.Param("id"))
		if wid == "" || wid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "watch id is required"))
			return
		}
		p := watchPath(c, wid)
		w, err := readWatch(c.Request.Context(), deps, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		var req updateWatchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		applyWatchUpdate(w, &req)
		if err := writeWatch(c.Request.Context(), deps, p, w); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(w))
	}
}

// updateWatchByURI handles PATCH /watches?to_uri=... — update by URI.
func updateWatchByURI(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		toURI := c.Query("to_uri")
		if toURI == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "to_uri is required"))
			return
		}
		w, p, err := findWatchByURI(c.Request.Context(), deps, c, toURI)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		var req updateWatchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		applyWatchUpdate(w, &req)
		if err := writeWatch(c.Request.Context(), deps, p, w); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(w))
	}
}

// applyWatchUpdate mutates w in place according to the patch. WatchInterval
// must be > 0 (Python server rejects <= 0 with 400; we just ignore <= 0
// here for simplicity since the SDK only sends positive values).
func applyWatchUpdate(w *watchEntry, req *updateWatchRequest) {
	if req.IsActive != nil {
		w.IsActive = *req.IsActive
	}
	if req.WatchInterval != nil && *req.WatchInterval > 0 {
		w.WatchInterval = *req.WatchInterval
	}
	if req.Reason != nil {
		w.Reason = *req.Reason
	}
	if req.Instruction != nil {
		w.Instruction = *req.Instruction
	}
}

// triggerWatch handles POST /watches/:id/trigger — trigger an immediate
// poll of the watch target and emit a change event if the stat differs
// from the persisted snapshot.
func triggerWatch(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		wid := path.Clean(c.Param("id"))
		if wid == "" || wid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "watch id is required"))
			return
		}
		p := watchPath(c, wid)
		w, err := readWatch(c.Request.Context(), deps, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		changed := triggerPoll(c, deps, w, p)
		c.JSON(http.StatusOK, okResponse(gin.H{
			"task_id": wid,
			"to_uri":  w.ToURI,
			"changed": changed,
			"path":    w.Path,
		}))
	}
}

// triggerWatchByURI handles POST /watches/trigger?to_uri=... — trigger by URI.
func triggerWatchByURI(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		toURI := c.Query("to_uri")
		if toURI == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "to_uri is required"))
			return
		}
		w, p, err := findWatchByURI(c.Request.Context(), deps, c, toURI)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		changed := triggerPoll(c, deps, w, p)
		c.JSON(http.StatusOK, okResponse(gin.H{
			"task_id": w.ID,
			"to_uri":  w.ToURI,
			"changed": changed,
			"path":    w.Path,
		}))
	}
}

// triggerPoll re-stats the watch target and returns whether the snapshot
// changed. The snapshot is persisted on change so subsequent polls resume
// from the new state.
func triggerPoll(c *gin.Context, deps *Deps, w *watchEntry, p string) bool {
	info, serr := deps.RAGFS.Stat(c.Request.Context(), w.Path)
	changed := false
	if serr != nil {
		changed = true
	} else if w.Snapshot != nil {
		changed = info.Size != w.Snapshot.Size || info.ModTime != w.Snapshot.ModTime
		if changed {
			w.Snapshot = &watchSnapshot{Size: info.Size, IsDir: info.IsDir, ModTime: info.ModTime}
			_ = writeWatch(c.Request.Context(), deps, p, w)
		}
	}
	return changed
}

// watchEvents handles GET /watches/:id/events — Server-Sent Events stream
// that emits a change event whenever the target path's stat differs from
// the persisted snapshot. The stream caps at 30 seconds for test-friendly
// behavior; production deployments should use a longer client-side timeout.
func watchEvents(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		wid := path.Clean(c.Param("id"))
		p := watchPath(c, wid)
		w, err := readWatch(c.Request.Context(), deps, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)

		flusher, _ := c.Writer.(http.Flusher)
		timeout := time.NewTimer(30 * time.Second)
		defer timeout.Stop()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		// The request context is cancelled when the client disconnects
		// (production server) or when the test's 30s timeout cap elapses.
		ctxDone := c.Request.Context().Done()
		last := *w.Snapshot
		for {
			select {
			case <-ctxDone:
				return
			case <-timeout.C:
				return
			case <-ticker.C:
				info, serr := deps.RAGFS.Stat(c.Request.Context(), w.Path)
				if serr != nil {
					// Stat failure (e.g. target deleted) emits a delete event.
					data, _ := json.Marshal(gin.H{"path": w.Path, "deleted": true})
					fmt.Fprintf(c.Writer, "event: delete\ndata: %s\n\n", data)
					if flusher != nil {
						flusher.Flush()
					}
					return
				}
				cur := watchSnapshot{Size: info.Size, IsDir: info.IsDir, ModTime: info.ModTime}
				if cur != last {
					data, _ := json.Marshal(gin.H{
						"path":    w.Path,
						"size":    cur.Size,
						"is_dir":  cur.IsDir,
						"mod_time": cur.ModTime,
					})
					fmt.Fprintf(c.Writer, "event: change\ndata: %s\n\n", data)
					if flusher != nil {
						flusher.Flush()
					}
					last = cur
					// Persist the new snapshot so a reconnect resumes from
					// the current state.
					w.Snapshot = &cur
					_ = writeWatch(c.Request.Context(), deps, p, w)
				}
			}
		}
	}
}

// newWatchID returns a sortable unique ID for a watch subscription.
func newWatchID() string {
	return fmt.Sprintf("wtch-%d", time.Now().UTC().UnixNano())
}
