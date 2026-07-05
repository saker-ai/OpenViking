package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterSnapshot wires /api/v1/snapshot/* — point-in-time snapshots.
//
// A snapshot is a recursive copy of the caller's resources root into
// /accounts/{account}/snapshots/{snap_id}/. Restore performs the reverse
// copy: the snapshot tree replaces the current resources root.
func RegisterSnapshot(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/snapshot")
	r.POST("", createSnapshot(deps))
	r.GET("", listSnapshots(deps))
	r.GET("/:id", getSnapshot(deps))
	r.POST("/:id/restore", restoreSnapshot(deps))
	r.DELETE("/:id", deleteSnapshot(deps))
}

// snapshotDir returns the ragfs directory holding the caller's snapshots.
func snapshotDir(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "snapshots"))
	}
	return "/snapshots"
}

func snapshotPath(c *gin.Context, id string) string {
	return ragfs.Normalize(path.Join(snapshotDir(c), id))
}

// snapshotCreateRequest is the JSON body for POST /snapshot.
type snapshotCreateRequest struct {
	// ID is the snapshot identifier. When empty the server generates one
	// from the current UTC timestamp (snap-<unix-millis>).
	ID string `json:"id"`
}

// snapshotMeta is the JSON metadata file written into each snapshot dir.
type snapshotMeta struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Source    string    `json:"source"`
}

// createSnapshot handles POST /snapshot — recursive-copy the resources root
// into a fresh snapshot directory.
func createSnapshot(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req snapshotCreateRequest
		_ = c.ShouldBindJSON(&req)
		id := req.ID
		if id == "" {
			id = "snap-" + fmt.Sprintf("%d", time.Now().UTC().UnixMilli())
		}
		snapPath := snapshotPath(c, id)
		if _, err := deps.RAGFS.Stat(c.Request.Context(), snapPath); err == nil {
			abortWithError(c, domain.ErrConflict.WithDetail("id", id))
			return
		}
		if err := deps.RAGFS.Mkdir(c.Request.Context(), snapPath, 0o755); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		src := resourcesRoot(c)
		if err := copyTree(c.Request.Context(), deps.RAGFS, src, snapPath); err != nil {
			_ = deps.RAGFS.Remove(c.Request.Context(), snapPath, true)
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		meta := snapshotMeta{ID: id, CreatedAt: time.Now().UTC(), Source: src}
		payload, _ := json.Marshal(meta)
		metaPath := ragfs.Normalize(path.Join(snapPath, ".snapshot.json"))
		_ = deps.RAGFS.Write(c.Request.Context(), metaPath, bytes.NewReader(payload), 0o644)
		c.JSON(http.StatusCreated, gin.H{"id": id, "path": snapPath, "source": src})
	}
}

// listSnapshots handles GET /snapshot — list snapshot IDs.
func listSnapshots(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		dir := snapshotDir(c)
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), dir)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, gin.H{"snapshots": []any{}, "path": dir})
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			if e == nil || e.Info == nil || !e.Info.IsDir {
				continue
			}
			ids = append(ids, e.Info.Name)
		}
		c.JSON(http.StatusOK, gin.H{"snapshots": ids, "path": dir})
	}
}

// getSnapshot handles GET /snapshot/:id — return snapshot metadata.
func getSnapshot(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		snapPath := snapshotPath(c, id)
		info, err := deps.RAGFS.Stat(c.Request.Context(), snapPath)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		meta := readSnapshotMeta(deps, c, snapPath)
		c.JSON(http.StatusOK, gin.H{
			"id":   id,
			"path": snapPath,
			"info": info,
			"meta": meta,
		})
	}
}

// restoreSnapshot handles POST /snapshot/:id/restore — copy the snapshot
// tree back into the resources root. The current resources root is removed
// (recursive) before the copy so the restore is exact.
func restoreSnapshot(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		snapPath := snapshotPath(c, id)
		if _, err := deps.RAGFS.Stat(c.Request.Context(), snapPath); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		dst := resourcesRoot(c)
		// Best-effort wipe of the current resources root. Missing-target
		// errors are fine — the snapshot will populate a fresh root.
		if err := deps.RAGFS.Remove(c.Request.Context(), dst, true); err != nil && !ragfs.IsNotFound(err) {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if err := copyTree(c.Request.Context(), deps.RAGFS, snapPath, dst); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "restored_to": dst, "source": snapPath})
	}
}

// deleteSnapshot handles DELETE /snapshot/:id — remove a snapshot recursively.
func deleteSnapshot(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		snapPath := snapshotPath(c, id)
		if err := deps.RAGFS.Remove(c.Request.Context(), snapPath, true); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "deleted": true})
	}
}

// readSnapshotMeta reads the .snapshot.json sidecar from a snapshot dir.
// Returns nil when the sidecar is absent (e.g. snapshots created by an
// older version that did not write metadata).
func readSnapshotMeta(deps *Deps, c *gin.Context, snapPath string) *snapshotMeta {
	metaPath := ragfs.Normalize(path.Join(snapPath, ".snapshot.json"))
	var buf bytes.Buffer
	if err := deps.RAGFS.Read(c.Request.Context(), metaPath, &buf); err != nil {
		return nil
	}
	var m snapshotMeta
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		return nil
	}
	return &m
}

// copyTree recursively copies every file under src into dst, preserving
// relative paths. Source directories are recreated via Mkdir before their
// children are copied. The src root itself is not re-created as a child of
// dst — dst takes its place. Files are copied byte-for-byte via the
// ragfs Copy method.
func copyTree(ctx context.Context, fs ragfs.FileSystem, src, dst string) error {
	entries, err := fs.ReadDir(ctx, src)
	if err != nil {
		if ragfs.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e == nil || e.Info == nil {
			continue
		}
		childSrc := ragfs.Normalize(path.Join(src, e.Info.Name))
		childDst := ragfs.Normalize(path.Join(dst, e.Info.Name))
		if e.Info.IsDir {
			if err := fs.Mkdir(ctx, childDst, e.Info.Mode); err != nil {
				if ragfs.IsConflict(err) {
					// dst already exists; reuse it.
				} else {
					return err
				}
			}
			if err := copyTree(ctx, fs, childSrc, childDst); err != nil {
				return err
			}
			continue
		}
		if err := fs.Copy(ctx, childSrc, childDst); err != nil {
			return err
		}
	}
	return nil
}
