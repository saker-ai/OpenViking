package routers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/webdav"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
)

// RegisterWebDAV wires /webdav/resources/* — a WebDAV server backed by the
// configured RAGFS. When RAGFS is nil the route falls back to the 501 stub so
// the server still boots and the route stays visible.
//
// The implementation adapts ragfs.FileSystem (POSIX paths with separate
// Read/Write calls and no random access) to golang.org/x/net/webdav.FileSystem
// (OpenFile with flags, webdav.File with Read/Write/Seek/Readdir/Stat).
// Read/Write payloads are buffered in memory per request because RAGFS does
// not expose random access; this is acceptable for the document-sized files
// RAGFS stores. LOCK/UNLOCK/PROPPATCH are handled by the in-memory lock
// system; dead properties are not persisted.
//
// WebDAV defines methods outside gin's Any set (MKCOL, PROPFIND, PROPPATCH,
// COPY, MOVE, LOCK, UNLOCK), so each method is registered explicitly via
// r.Handle. gin's radix tree holds a single catch-all node *any under
// /webdav/resources which matches every sub-path.
func RegisterWebDAV(r *gin.Engine, deps *Deps) {
	if deps == nil || deps.RAGFS == nil {
		r.Any("/webdav/resources/*any", stub("webdav"))
		return
	}
	handler := &webdav.Handler{
		Prefix:     "/webdav/resources",
		FileSystem: adaptRAGFSToWebDAV(deps.RAGFS),
		LockSystem: webdav.NewMemLS(),
	}
	wrapped := gin.WrapH(handler)
	// gin's AnyMethods does not include WebDAV-specific verbs, so register
	// the union of standard HTTP methods and WebDAV methods explicitly.
	for _, m := range webdavMethods {
		r.Handle(m, "/webdav/resources/*any", wrapped)
	}
}

// webdavMethods is the union of HTTP methods gin's Any covers plus the
// WebDAV-specific methods defined by RFC 4918 / RFC 5323.
var webdavMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
	"MKCOL",
	"COPY",
	"MOVE",
	"LOCK",
	"UNLOCK",
	"PROPFIND",
	"PROPPATCH",
}

// ragfsWebDAVFS adapts ragfs.FileSystem to the webdav.FileSystem interface.
// webdav passes POSIX paths with a leading slash (already cleaned by
// slashClean), which matches ragfs's path convention.
type ragfsWebDAVFS struct {
	ragfs ragfs.FileSystem
}

func adaptRAGFSToWebDAV(rfs ragfs.FileSystem) *ragfsWebDAVFS {
	return &ragfsWebDAVFS{ragfs: rfs}
}

// Mkdir creates a directory at name with the given permission.
func (w *ragfsWebDAVFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if perm == 0 {
		perm = 0o755
	}
	if err := w.ragfs.Mkdir(ctx, name, perm); err != nil {
		return translateRAGFSError(err)
	}
	return nil
}

// OpenFile opens name with the given flag/perm. webdav uses os package flags
// (O_RDONLY, O_WRONLY, O_RDWR, O_CREATE, O_TRUNC, O_APPEND). The returned
// webdav.File buffers content in memory.
//
// When O_CREATE is set and the file does not yet exist, an empty file is
// created immediately so subsequent Stat calls (webdav's handlePut calls
// Stat before Close flushes the write buffer) succeed rather than 404.
func (w *ragfsWebDAVFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	st, statErr := w.ragfs.Stat(ctx, name)
	if statErr != nil && !ragfs.IsNotFound(statErr) {
		return nil, translateRAGFSError(statErr)
	}
	if st != nil && st.IsDir {
		// Directories are read-only via OpenFile; MKCOL goes through Mkdir.
		return &ragfsWebDAVFile{
			fs:    w,
			path:  name,
			flag:  flag,
			perm:  perm,
			info:  st,
			isDir: true,
		}, nil
	}
	if st == nil && flag&os.O_CREATE != 0 {
		// Touch the file so Stat calls before Close succeed.
		if err := w.ragfs.Write(ctx, name, bytes.NewReader(nil), filePerm(perm)); err != nil {
			return nil, translateRAGFSError(err)
		}
		st, statErr = w.ragfs.Stat(ctx, name)
		if statErr != nil {
			return nil, translateRAGFSError(statErr)
		}
	}
	return &ragfsWebDAVFile{
		fs:   w,
		path: name,
		flag: flag,
		perm: filePerm(perm),
		info: st,
	}, nil
}

// RemoveAll removes name recursively (ragfs.Remove with recursive=true).
func (w *ragfsWebDAVFS) RemoveAll(ctx context.Context, name string) error {
	if err := w.ragfs.Remove(ctx, name, true); err != nil {
		return translateRAGFSError(err)
	}
	return nil
}

// Rename moves oldName to newName.
func (w *ragfsWebDAVFS) Rename(ctx context.Context, oldName, newName string) error {
	if err := w.ragfs.Rename(ctx, oldName, newName); err != nil {
		return translateRAGFSError(err)
	}
	return nil
}

// Stat returns os.FileInfo for name. Missing paths return os.ErrNotExist so
// the webdav handler emits 404.
func (w *ragfsWebDAVFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	st, err := w.ragfs.Stat(ctx, name)
	if err != nil {
		return nil, translateRAGFSError(err)
	}
	return &ragfsFileInfo{fi: st}, nil
}

// ragfsWebDAVFile implements webdav.File by buffering content in memory.
// webdav.File embeds http.File (io.Closer, io.Reader, io.Seeker, Readdir,
// Stat) and adds io.Writer.
type ragfsWebDAVFile struct {
	fs    *ragfsWebDAVFS
	path  string
	flag  int
	perm  os.FileMode
	info  *ragfs.FileInfo
	isDir bool

	// read-side state (lazy-loaded on first Read/Seek)
	readBuf    []byte
	readPos    int
	readLoaded bool

	// write-side state (buffered until Close)
	writeBuf bytes.Buffer

	// directory listing state (lazy-loaded on first Readdir)
	entries   []*ragfs.TreeEntry
	dirPos    int
	dirLoaded bool

	closed bool
}

// Close flushes buffered writes to ragfs. Read-only handles need no flushing.
func (f *ragfsWebDAVFile) Close() error {
	if f == nil || f.closed {
		return nil
	}
	f.closed = true
	if f.isDir {
		return nil
	}
	if f.flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		return nil
	}
	// O_APPEND is a no-op for ragfs since Write always replaces the whole
	// file; clients that open+append would have already read the existing
	// content via O_RDWR. This matches the standard webdav PUT shape where
	// the handler opens with O_WRONLY|O_CREATE|O_TRUNC.
	if err := f.fs.ragfs.Write(context.Background(), f.path, &f.writeBuf, f.perm); err != nil {
		return translateRAGFSError(err)
	}
	return nil
}

// Read implements io.Reader. The file content is loaded once on first read.
func (f *ragfsWebDAVFile) Read(p []byte) (int, error) {
	if f.isDir {
		return 0, os.ErrInvalid
	}
	if f.flag&os.O_WRONLY != 0 {
		return 0, os.ErrInvalid
	}
	if err := f.ensureReadLoaded(); err != nil {
		return 0, err
	}
	if f.readPos >= len(f.readBuf) {
		return 0, io.EOF
	}
	n := copy(p, f.readBuf[f.readPos:])
	f.readPos += n
	return n, nil
}

// Seek implements io.Seeker. webdav calls Seek for Range support and
// ServeContent; we satisfy it over the in-memory buffer.
func (f *ragfsWebDAVFile) Seek(offset int64, whence int) (int64, error) {
	if f.isDir {
		return 0, os.ErrInvalid
	}
	if err := f.ensureReadLoaded(); err != nil {
		return 0, err
	}
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = int64(f.readPos) + offset
	case io.SeekEnd:
		newPos = int64(len(f.readBuf)) + offset
	default:
		return 0, errors.New("webdav: invalid whence")
	}
	if newPos < 0 {
		return 0, os.ErrInvalid
	}
	if newPos > int64(len(f.readBuf)) {
		newPos = int64(len(f.readBuf))
	}
	f.readPos = int(newPos)
	return newPos, nil
}

// Write implements io.Writer. Bytes are buffered and flushed on Close.
func (f *ragfsWebDAVFile) Write(p []byte) (int, error) {
	if f.isDir {
		return 0, os.ErrInvalid
	}
	if f.flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		return 0, os.ErrInvalid
	}
	return f.writeBuf.Write(p)
}

// Stat returns os.FileInfo for the file. The ragfs.Stat result is cached at
// OpenFile time when available; otherwise it's fetched lazily. When the
// write buffer holds in-flight content (webdav's handlePut calls Stat after
// io.Copy but before Close flushes), the FileInfo is synthesized from the
// buffer so size/modtime reflect the new content rather than the stale
// ragfs state.
func (f *ragfsWebDAVFile) Stat() (fs.FileInfo, error) {
	if f.isDir {
		if f.info == nil {
			st, err := f.fs.ragfs.Stat(context.Background(), f.path)
			if err != nil {
				return nil, translateRAGFSError(err)
			}
			f.info = st
		}
		return &ragfsFileInfo{fi: f.info}, nil
	}
	if f.writeBuf.Len() > 0 {
		return &ragfsFileInfo{fi: &ragfs.FileInfo{
			Name:    path.Base(f.path),
			Size:    int64(f.writeBuf.Len()),
			Mode:    filePerm(f.perm),
			ModTime: time.Now(),
			IsDir:   false,
		}}, nil
	}
	if f.info == nil {
		st, err := f.fs.ragfs.Stat(context.Background(), f.path)
		if err != nil {
			return nil, translateRAGFSError(err)
		}
		f.info = st
	}
	return &ragfsFileInfo{fi: f.info}, nil
}

// Readdir returns directory entries. webdav calls Readdir on directories
// during PROPFIND with depth>=1; count<=0 means "all entries".
func (f *ragfsWebDAVFile) Readdir(count int) ([]fs.FileInfo, error) {
	if !f.isDir {
		return nil, os.ErrInvalid
	}
	if err := f.ensureDirLoaded(); err != nil {
		return nil, err
	}
	if count <= 0 {
		out := make([]fs.FileInfo, 0, len(f.entries))
		for _, e := range f.entries {
			if e == nil || e.Info == nil {
				continue
			}
			out = append(out, &ragfsFileInfo{fi: e.Info})
		}
		f.dirPos = len(f.entries)
		return out, nil
	}
	if f.dirPos >= len(f.entries) {
		return nil, io.EOF
	}
	end := f.dirPos + count
	if end > len(f.entries) {
		end = len(f.entries)
	}
	out := make([]fs.FileInfo, 0, end-f.dirPos)
	for _, e := range f.entries[f.dirPos:end] {
		if e == nil || e.Info == nil {
			continue
		}
		out = append(out, &ragfsFileInfo{fi: e.Info})
	}
	f.dirPos = end
	if f.dirPos >= len(f.entries) {
		return out, io.EOF
	}
	return out, nil
}

// ensureReadLoaded reads the file content from ragfs into readBuf. Skipped
// for write-only handles where Read would never be called.
func (f *ragfsWebDAVFile) ensureReadLoaded() error {
	if f.readLoaded {
		return nil
	}
	// For write-only handles we still load the read buffer when the caller
	// invokes Seek/Read; this supports O_RDWR semantics where the client
	// reads then writes.
	var buf bytes.Buffer
	if err := f.fs.ragfs.Read(context.Background(), f.path, &buf); err != nil {
		return translateRAGFSError(err)
	}
	f.readBuf = buf.Bytes()
	f.readLoaded = true
	return nil
}

// ensureDirLoaded reads the directory entries from ragfs.
func (f *ragfsWebDAVFile) ensureDirLoaded() error {
	if f.dirLoaded {
		return nil
	}
	entries, err := f.fs.ragfs.ReadDir(context.Background(), f.path)
	if err != nil {
		return translateRAGFSError(err)
	}
	f.entries = entries
	f.dirLoaded = true
	return nil
}

// ragfsFileInfo wraps ragfs.FileInfo to satisfy the os.FileInfo / fs.FileInfo
// interface. ragfs.FileInfo is a plain struct; webdav (and net/http) call
// the methods to render directory listings and PROPFIND responses.
type ragfsFileInfo struct {
	fi *ragfs.FileInfo
}

func (r *ragfsFileInfo) Name() string       { return r.fi.Name }
func (r *ragfsFileInfo) Size() int64        { return r.fi.Size }
func (r *ragfsFileInfo) Mode() os.FileMode  { return r.fi.Mode }
func (r *ragfsFileInfo) ModTime() time.Time { return r.fi.ModTime }
func (r *ragfsFileInfo) IsDir() bool        { return r.fi.IsDir }
func (r *ragfsFileInfo) Sys() interface{}   { return r.fi }

// filePerm normalizes the perm passed to OpenFile. webdav passes 0 when the
// file already exists; ragfs.Write requires a non-zero perm.
func filePerm(p os.FileMode) os.FileMode {
	if p == 0 {
		return 0o644
	}
	return p
}

// translateRAGFSError maps a ragfs error to one webdav can act on. Missing
// paths become os.ErrNotExist (so the handler emits 404); other ragfs errors
// pass through wrapped in domain.ErrRAGFS for the error middleware to render.
func translateRAGFSError(err error) error {
	if err == nil {
		return nil
	}
	if ragfs.IsNotFound(err) {
		return os.ErrNotExist
	}
	if ragfs.IsConflict(err) {
		return os.ErrExist
	}
	// webdav only checks errors.Is against os.Err*; everything else is
	// surfaced as a 500 by the handler. Wrap so the domain code survives.
	return domain.Wrap(domain.CodeRAGFSError, 500, err)
}
