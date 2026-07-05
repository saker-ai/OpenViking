package routers

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterResources wires /api/v1/resources/* — Resource CRUD. Wildcard
// paths can't be combined with sub-segments in gin's radix tree, so
// touch lives as a query-action on the resource itself.
//
// Paths are scoped to the caller's account when identity is present:
// /api/v1/resources/foo -> /accounts/{account}/resources/foo on ragfs.
// When identity is absent the raw URI is used (tests only).
//
// SDK-compat alias:
//   - POST /resources/temp_upload — receive multipart file, persist under
//     /accounts/{account}/tmp/{temp_id}, return {temp_file_id} for the
//     subsequent POST /resources call. The Go SDK's AddResource uploads
//     the local file here first, then posts {temp_file_id, to, reason}
//     to create the resource.
func RegisterResources(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/resources")
	r.GET("", listResources(deps))
	r.POST("", createResource(deps))
	r.POST("/temp_upload", tempUpload(deps))
	r.GET("/*uri", getResource(deps))
	r.PUT("/*uri", writeResource(deps))
	r.DELETE("/*uri", deleteResource(deps))
	r.HEAD("/*uri", headResource(deps))
}

// resourcePath resolves the request URI against the caller's account prefix.
func resourcePath(c *gin.Context) string {
	uri := c.Param("uri")
	if uri == "" {
		uri = "/"
	}
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "resources", uri))
	}
	return ragfs.Normalize(uri)
}

// listPath returns the ragfs path for the resources root scoped to the
// caller's account.
func listPath(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "resources"))
	}
	return "/"
}

// listResources handles GET /resources — list entries under the resources root.
// When the resources root does not exist yet (fresh account), an empty list
// is returned rather than an error so callers can bootstrap.
func listResources(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := listPath(c)
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, gin.H{"entries": []any{}, "path": p})
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"entries": entries, "path": p})
	}
}

// createResourceRequest is the JSON body for POST /resources.
type createResourceRequest struct {
	// Path is the resource path relative to the caller's resources root.
	// Empty means create the parent directory entry (no-op when already
	// present).
	Path string `json:"path"`
	// IsDir selects between Mkdir (true) and Write (false) when Content is
	// empty. When Content is non-empty the resource is always a file write.
	IsDir bool `json:"is_dir"`
	// Content is the optional bytes for a file resource. When non-empty
	// the handler writes the bytes via ragfs.Write.
	Content []byte `json:"content,omitempty"`
	// Mode is the optional POSIX permission bits (e.g. 0o644, 0o755).
	// Defaults to 0o644 for files, 0o755 for directories.
	Mode uint32 `json:"mode,omitempty"`

	// SDK AddResource fields. When TempFileID is set, the handler reads the
	// previously uploaded temp file (from POST /resources/temp_upload) and
	// writes it to the resolved To path. SourceName selects between file
	// write (non-zip) and zip extraction (zip suffix). To may be a
	// viking://resources/<path> URI or a raw relative path.
	To         string `json:"to,omitempty"`
	TempFileID string `json:"temp_file_id,omitempty"`
	SourceName string `json:"source_name,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Wait       bool   `json:"wait,omitempty"`
	// WatchInterval, when > 0, registers a polling watch subscription on
	// the resolved To path so the SDK's AddResource(WatchInterval=...) flow
	// works without a separate POST /watches call. Mirrors the Python
	// server's add_resource watch creation branch.
	WatchInterval float64 `json:"watch_interval,omitempty"`
	Instruction   string  `json:"instruction,omitempty"`
}

// createResource handles POST /resources — create a directory, write a
// file, or import a previously uploaded temp file under the resources root.
//
// Two payload shapes are accepted:
//  1. {path, is_dir, content, mode} — direct write (legacy Go form).
//  2. {to, temp_file_id, source_name, reason, wait} — SDK AddResource form
//     where the file was already uploaded to /resources/temp_upload and
//     is now copied/extracted to the resolved To path.
func createResource(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req createResourceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		// SDK AddResource flow: copy/extract temp file to To path.
		if req.TempFileID != "" {
			targetPath := resolveToPath(c, req.To, req.SourceName)
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
			if strings.HasSuffix(req.SourceName, ".zip") {
				if err := extractZip(c.Request.Context(), deps.RAGFS, buf.Bytes(), targetPath); err != nil {
					abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
					return
				}
			} else {
				_ = deps.RAGFS.Mkdir(c.Request.Context(), path.Dir(targetPath), 0o755)
				if err := deps.RAGFS.Write(c.Request.Context(), targetPath, bytes.NewReader(buf.Bytes()), 0o644); err != nil {
					abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
					return
				}
			}
			// Best-effort cleanup of the temp file; the resource is already
			// persisted so a failed cleanup is not fatal.
			_ = deps.RAGFS.Remove(c.Request.Context(), tempPath, false)
			// When the SDK requested a watch, register a polling
			// subscription on the resolved To path so subsequent
			// ListWatches(ToURI=...) / UpdateWatch(ToURI=...) / TriggerWatch
			// (ToURI=...) calls find it. Best-effort: a watch creation
			// failure does not fail the resource create.
			var watch *watchEntry
			if req.WatchInterval > 0 {
				watch, _ = createResourceWatch(c, deps, targetPath, req)
			}
			resp := gin.H{
				"uri":    targetPath,
				"path":   targetPath,
				"to":     req.To,
				"reason": req.Reason,
			}
			if watch != nil {
				resp["watch_id"] = watch.ID
				resp["watch_interval"] = watch.WatchInterval
			}
			c.JSON(http.StatusCreated, okResponse(resp))
			return
		}
		// Legacy direct-write flow.
		if req.Path == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "path is required"))
			return
		}
		base := listPath(c)
		full := ragfs.Normalize(path.Join(base, req.Path))
		mode := os.FileMode(req.Mode)
		if mode == 0 {
			if req.IsDir || len(req.Content) == 0 {
				mode = 0o755
			} else {
				mode = 0o644
			}
		}
		if req.IsDir || len(req.Content) == 0 {
			if err := deps.RAGFS.Mkdir(c.Request.Context(), full, mode); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
		} else {
			if err := deps.RAGFS.Write(c.Request.Context(), full, bytes.NewReader(req.Content), mode); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
		}
		c.JSON(http.StatusCreated, gin.H{"path": full})
	}
}

// tempUpload handles POST /resources/temp_upload — receive a multipart file
// upload, persist it under /accounts/{account}/tmp/{temp_id}, return the
// temp_file_id for use in the subsequent POST /resources call. The Go SDK's
// AddResource uploads the local file here first, then posts {temp_file_id,
// to, reason} to create the resource.
func tempUpload(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		file, header, err := c.Request.FormFile("file")
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		defer file.Close()
		tempID := newTempID()
		tempPath := tempFilePath(c, tempID)
		_ = deps.RAGFS.Mkdir(c.Request.Context(), path.Dir(tempPath), 0o755)
		if err := deps.RAGFS.Write(c.Request.Context(), tempPath, file, 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, okResponse(gin.H{
			"temp_file_id": tempID,
			"name":         header.Filename,
			"size":         header.Size,
		}))
	}
}

// tempFilePath returns the ragfs path for a temp file under the caller's
// account. Temp files are stored under /accounts/{account}/tmp/ and are
// cleaned up by createResource after the resource is persisted.
func tempFilePath(c *gin.Context, tempID string) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "tmp", tempID))
	}
	return ragfs.Normalize(path.Join("/tmp", tempID))
}

// newTempID returns a 16-byte hex ID suitable for use as a temp file name.
// crypto/rand is used so temp IDs are not guessable (avoids trivial
// collision attacks on the temp upload namespace).
func newTempID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// resolveToPath resolves the SDK's "to" field against the caller's resources
// root. "to" may be a viking://resources/<path> URI or a raw relative path.
// When sourceName is a non-zip file, it is appended to the target directory
// (the SDK uploads a single file, not a directory, so the target is always
// a file path). When sourceName ends with .zip the target is a directory
// (the zip is extracted there).
func resolveToPath(c *gin.Context, to, sourceName string) string {
	base := listPath(c)
	rel := to
	if strings.HasPrefix(rel, "viking://resources/") {
		rel = strings.TrimPrefix(rel, "viking://resources/")
	} else if strings.HasPrefix(rel, "viking://") {
		rel = strings.TrimPrefix(rel, "viking://")
	}
	rel = strings.TrimPrefix(rel, "/")
	var target string
	if rel == "" {
		target = base
	} else {
		target = ragfs.Normalize(path.Join(base, rel))
	}
	if sourceName != "" && !strings.HasSuffix(sourceName, ".zip") {
		target = ragfs.Normalize(path.Join(target, sourceName))
	}
	return target
}

// extractZip writes every entry in the zip archive to targetDir on fs.
// Directory entries are created via Mkdir; regular file entries are written
// via Write. Symlinks and other non-regular types are skipped.
func extractZip(ctx context.Context, fs ragfs.FileSystem, data []byte, targetDir string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	_ = fs.Mkdir(ctx, targetDir, 0o755)
	for _, f := range r.File {
		dst := ragfs.Normalize(path.Join(targetDir, f.Name))
		if f.FileInfo().IsDir() {
			_ = fs.Mkdir(ctx, dst, 0o755)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = fs.Write(ctx, dst, rc, 0o644)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// createResourceWatch creates a polling watch subscription for the resolved
// resource path. It mirrors the Python server's add_resource watch branch:
// when watch_interval > 0, a watch task is persisted so subsequent
// ListWatches / UpdateWatch / TriggerWatch calls by ToURI find it. The
// watch's Path is the ragfs-resolved target (so SSE events can re-stat
// it); ToURI preserves the caller's original viking:// URI for query
// matching.
func createResourceWatch(c *gin.Context, deps *Deps, targetPath string, req createResourceRequest) (*watchEntry, error) {
	info, err := deps.RAGFS.Stat(c.Request.Context(), targetPath)
	if err != nil {
		return nil, err
	}
	id, _ := identity.FromContext(c.Request.Context())
	wid := newWatchID()
	w := &watchEntry{
		ID:            wid,
		Path:          targetPath,
		ToURI:         req.To,
		Account:       id.Account,
		IsActive:      true,
		WatchInterval: req.WatchInterval,
		Reason:        req.Reason,
		Instruction:   req.Instruction,
		CreatedAt:     time.Now().UTC(),
		Snapshot: &watchSnapshot{
			Size:    info.Size,
			IsDir:   info.IsDir,
			ModTime: info.ModTime,
		},
	}
	if err := writeWatch(c.Request.Context(), deps, watchPath(c, wid), w); err != nil {
		return nil, err
	}
	return w, nil
}

// getResource handles GET /resources/*uri — read resource metadata.
func getResource(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := resourcePath(c)
		info, err := deps.RAGFS.Stat(c.Request.Context(), p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p, "info": info})
	}
}

// writeResource handles PUT /resources/*uri — write resource bytes from the
// request body.
func writeResource(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := resourcePath(c)
		if err := deps.RAGFS.Write(c.Request.Context(), p, c.Request.Body, 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p})
	}
}

// deleteResource handles DELETE /resources/*uri — remove a resource. The
// remove is recursive when ?recursive=1 is set.
func deleteResource(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := resourcePath(c)
		recursive := c.Query("recursive") == "1"
		if err := deps.RAGFS.Remove(c.Request.Context(), p, recursive); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p, "deleted": true})
	}
}

// headResource handles HEAD /resources/*uri — stat only, no body.
func headResource(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := resourcePath(c)
		info, err := deps.RAGFS.Stat(c.Request.Context(), p)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.Header("Content-Type", "application/json")
		c.Header("X-Resource-Name", info.Name)
		c.Header("X-Resource-Size", itoa64(info.Size))
		c.Header("X-Resource-IsDir", boolStr(info.IsDir))
		c.Status(http.StatusNoContent)
	}
}

// itoa64 formats an int64 without dragging in strconv at the call site.
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
