package routers

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterPack wires /api/v1/pack/* — ovpack export/import.
//
// A pack is a tar archive streamed to/from ragfs. Export reads a list of
// resource paths from the request body, packs every file into a tar stream,
// and either streams it to the client or saves it under
// /accounts/{account}/packs/{id} for later download. Import reads a tar
// stream from the request body and extracts every entry into the caller's
// resources root.
//
// SDK-compat aliases (Go SDK at sdk/go):
//   - POST /pack/backup   — backup public scopes (alias to exportPack with
//     paths defaulting to ["/"] when not supplied)
//   - POST /pack/restore  — restore from a previously uploaded pack (reads
//     the temp tar file uploaded via /resources/temp_upload)
func RegisterPack(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/pack")
	r.POST("/export", exportPack(deps))
	r.POST("/import", importPack(deps))
	r.POST("/backup", backupPack(deps))
	r.POST("/restore", restorePack(deps))
	r.GET("/:id", packStatus(deps))
	r.GET("/:id/download", downloadPack(deps))
}

// backupPackRequest is the JSON body for POST /pack/backup.
type backupPackRequest struct {
	IncludeVectors bool     `json:"include_vectors,omitempty"`
	Paths          []string `json:"paths,omitempty"`
	ID             string   `json:"id,omitempty"`
}

// backupPack handles POST /pack/backup — backup public scopes. The Go SDK
// posts {include_vectors}. When paths is empty, the entire resources root
// is backed up. Mirrors exportPack's streaming/persist behavior.
func backupPack(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req backupPackRequest
		_ = c.ShouldBindJSON(&req)
		root := resourcesRoot(c)
		paths := req.Paths
		if len(paths) == 0 {
			paths = []string{"/"}
		}
		resolved := make([]string, 0, len(paths))
		for _, p := range paths {
			resolved = append(resolved, resolvePackPath(root, p))
		}
		if req.ID == "" {
			c.Header("Content-Type", "application/x-tar")
			c.Header("Content-Disposition", "attachment; filename=backup.tar")
			c.Status(http.StatusOK)
			if err := streamPack(c.Request.Context(), deps.RAGFS, resolved, c.Writer); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			return
		}
		p := packPath(c, req.ID)
		var buf bytes.Buffer
		if err := streamPack(c.Request.Context(), deps.RAGFS, resolved, &buf); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if err := deps.RAGFS.Write(c.Request.Context(), p, bytes.NewReader(buf.Bytes()), 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, okResponse(gin.H{"id": req.ID, "path": p, "bytes": buf.Len()}))
	}
}

// restorePackRequest is the JSON body for POST /pack/restore.
type restorePackRequest struct {
	TempFileID string `json:"temp_file_id,omitempty"`
	OnConflict string `json:"on_conflict,omitempty"`
	VectorMode string `json:"vector_mode,omitempty"`
}

// restorePack handles POST /pack/restore — restore from a previously
// uploaded pack. The Go SDK uploads the file via /resources/temp_upload then
// posts {temp_file_id}. The temp file is a tar stream extracted into the
// caller's resources root, mirroring importPack's tar extraction logic.
func restorePack(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req restorePackRequest
		_ = c.ShouldBindJSON(&req)
		if req.TempFileID == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "temp_file_id is required"))
			return
		}
		root := resourcesRoot(c)
		tempPath := tempFilePath(c, req.TempFileID)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), tempPath, &buf); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
		var imported []string
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
				return
			}
			name := strings.TrimPrefix(hdr.Name, "/")
			if name == "" || strings.Contains(name, "..") {
				continue
			}
			dst := ragfs.Normalize(path.Join(root, name))
			if hdr.Typeflag == tar.TypeDir {
				if err := deps.RAGFS.Mkdir(c.Request.Context(), dst, 0o755); err != nil {
					abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
					return
				}
				imported = append(imported, dst+"/")
				continue
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			if err := deps.RAGFS.Write(c.Request.Context(), dst, tr, 0o644); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			imported = append(imported, dst)
		}
		_ = deps.RAGFS.Remove(c.Request.Context(), tempPath, false)
		c.JSON(http.StatusOK, okResponse(gin.H{
			"imported":     imported,
			"root":         root,
			"temp_file_id": req.TempFileID,
			"status":       "completed",
		}))
	}
}

// packDir returns the ragfs directory holding saved packs.
func packDir(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "packs"))
	}
	return "/packs"
}

func packPath(c *gin.Context, id string) string {
	return ragfs.Normalize(path.Join(packDir(c), id+".tar"))
}

// packExportRequest is the JSON body for POST /pack/export.
type packExportRequest struct {
	// Paths are ragfs paths (relative to the caller's resources root when
	// no leading slash) to include in the pack. Each path is resolved
	// against the resources root, so "docs/intro.md" packs the file at
	// /accounts/{account}/resources/docs/intro.md.
	Paths []string `json:"paths"`
	// ID is the optional pack identifier. When set the pack is saved under
	// /accounts/{account}/packs/{id}.tar for later download; when empty
	// the tar is streamed directly to the response.
	ID string `json:"id"`
}

// exportPack handles POST /pack/export — build a tar from the given paths
// and either stream it back or persist it under /packs/{id}.
func exportPack(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req packExportRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if len(req.Paths) == 0 {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "paths is required"))
			return
		}
		root := resourcesRoot(c)
		// Resolve every requested path against the resources root.
		resolved := make([]string, 0, len(req.Paths))
		for _, p := range req.Paths {
			resolved = append(resolved, resolvePackPath(root, p))
		}
		if req.ID == "" {
			// Stream directly to the response.
			c.Header("Content-Type", "application/x-tar")
			c.Header("Content-Disposition", "attachment; filename=pack.tar")
			c.Status(http.StatusOK)
			if err := streamPack(c.Request.Context(), deps.RAGFS, resolved, c.Writer); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			return
		}
		// Persist under /packs/{id}.tar.
		p := packPath(c, req.ID)
		var buf bytes.Buffer
		if err := streamPack(c.Request.Context(), deps.RAGFS, resolved, &buf); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if err := deps.RAGFS.Write(c.Request.Context(), p, bytes.NewReader(buf.Bytes()), 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{"id": req.ID, "path": p, "bytes": buf.Len()})
	}
}

// importPack handles POST /pack/import — extract a tar stream into the
// caller's resources root. Entries are written to
// /accounts/{account}/resources/{entry.Name}.
func importPack(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		root := resourcesRoot(c)
		tr := tar.NewReader(c.Request.Body)
		var imported []string
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
				return
			}
			name := strings.TrimPrefix(hdr.Name, "/")
			if name == "" || strings.Contains(name, "..") {
				continue
			}
			dst := ragfs.Normalize(path.Join(root, name))
			if hdr.Typeflag == tar.TypeDir {
				if err := deps.RAGFS.Mkdir(c.Request.Context(), dst, 0o755); err != nil {
					abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
					return
				}
				imported = append(imported, dst+"/")
				continue
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			if err := deps.RAGFS.Write(c.Request.Context(), dst, tr, 0o644); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			imported = append(imported, dst)
		}
		c.JSON(http.StatusOK, gin.H{"imported": imported, "root": root})
	}
}

// packStatus handles GET /pack/:id — stat a saved pack.
func packStatus(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		p := packPath(c, id)
		info, err := deps.RAGFS.Stat(c.Request.Context(), p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "path": p, "info": info})
	}
}

// downloadPack handles GET /pack/:id/download — stream a saved pack.
func downloadPack(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		p := packPath(c, id)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), p, &buf); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.Header("Content-Type", "application/x-tar")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s.tar", id))
		c.Data(http.StatusOK, "application/x-tar", buf.Bytes())
	}
}

// resolvePackPath resolves a user-supplied pack path against the resources
// root. Absolute paths are normalized as-is; relative paths are joined
// under the root.
func resolvePackPath(root, p string) string {
	if strings.HasPrefix(p, "/") {
		return ragfs.Normalize(p)
	}
	return ragfs.Normalize(path.Join(root, p))
}

// streamPack writes every file under the given paths into a tar stream
// written to w. Directories are walked recursively via ReadDir.
func streamPack(ctx context.Context, fs ragfs.FileSystem, paths []string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	for _, p := range paths {
		info, err := fs.Stat(ctx, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				continue
			}
			return err
		}
		if info.IsDir {
			if err := packDirToTar(ctx, fs, p, tw); err != nil {
				return err
			}
			continue
		}
		if err := packFileToTar(ctx, fs, p, info, tw); err != nil {
			return err
		}
	}
	return nil
}

// packDirToTar walks a directory recursively and writes every file entry to tw.
func packDirToTar(ctx context.Context, fs ragfs.FileSystem, p string, tw *tar.Writer) error {
	entries, err := fs.ReadDir(ctx, p)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e == nil || e.Info == nil {
			continue
		}
		child := ragfs.Normalize(path.Join(p, e.Info.Name))
		if e.Info.IsDir {
			if err := packDirToTar(ctx, fs, child, tw); err != nil {
				return err
			}
			continue
		}
		if err := packFileToTar(ctx, fs, child, e.Info, tw); err != nil {
			return err
		}
	}
	return nil
}

// packFileToTar reads a single file from ragfs and writes it as a tar entry.
func packFileToTar(ctx context.Context, fs ragfs.FileSystem, p string, info *ragfs.FileInfo, tw *tar.Writer) error {
	var buf bytes.Buffer
	if err := fs.Read(ctx, p, &buf); err != nil {
		return err
	}
	body := buf.Bytes()
	hdr := &tar.Header{
		Name:     strings.TrimPrefix(p, "/"),
		Typeflag: tar.TypeReg,
		Size:     int64(len(body)),
		ModTime:  info.ModTime,
		Mode:     int64(info.Mode),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	return nil
}

// packImportResult is the JSON shape returned by POST /pack/import. Kept
// as a named type so tests can unmarshal into it.
type packImportResult struct {
	Imported []string `json:"imported"`
	Root     string   `json:"root"`
}
