package ragfs

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// FileInfo describes a path entry returned by Stat / ReadDir.
type FileInfo struct {
	Name    string
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
	IsDir   bool
}

// TreeEntry is one row in a directory listing.
type TreeEntry struct {
	Path    string
	RelPath string
	Info    *FileInfo
	Extra   map[string]any
}

// GrepMatch is one hit from a Grep operation.
type GrepMatch struct {
	Path    string
	LineNo  int
	Line    string
	Pattern string
}

// PluginConfig carries backend-specific settings to ServicePlugin methods.
// Settings is a free-form map parsed from the ov.conf mount stanza.
type PluginConfig struct {
	Name     string
	Settings map[string]any
}

// ServicePlugin corresponds to the Rust trait core/plugin.rs::ServicePlugin.
// Every FileSystem backend must satisfy it so that mount tables can validate
// and lifecycle-manage their backends uniformly.
type ServicePlugin interface {
	Name() string
	Validate(cfg *PluginConfig) error
	Initialize(ctx context.Context, cfg *PluginConfig) error
	HealthCheck(ctx context.Context) error
}

// FileSystem is the contract every ragfs backend must implement.
//
// Paths passed to these methods are POSIX-style, percent-decoded by the
// caller, with a leading slash. Backends must return domain.ErrNotFound
// when a path is missing and domain.ErrConflict when a target already
// exists, so that callers can use errors.Is uniformly.
//
// Corresponds to crates/ragfs/src/core/filesystem.rs::FileSystem.
type FileSystem interface {
	ServicePlugin
	Create(ctx context.Context, path string, isDir bool) error
	Mkdir(ctx context.Context, path string, perm os.FileMode) error
	Remove(ctx context.Context, path string, recursive bool) error
	Read(ctx context.Context, path string, w io.Writer) error
	Write(ctx context.Context, path string, r io.Reader, perm os.FileMode) error
	ReadDir(ctx context.Context, path string) ([]*TreeEntry, error)
	Stat(ctx context.Context, path string) (*FileInfo, error)
	Rename(ctx context.Context, oldPath, newPath string) error
	Copy(ctx context.Context, srcPath, dstPath string) error
	Chmod(ctx context.Context, path string, perm os.FileMode) error
	Grep(ctx context.Context, pattern, path string, recursive bool) ([]GrepMatch, error)
	TreeDirectory(ctx context.Context, path string, depth int) ([]*TreeEntry, error)
}

// Sentinel errors.
//
// Backends return domain.ErrNotFound for missing paths and
// domain.ErrConflict for already-exists so that errors.Is works against
// the domain sentinels. ErrUnsupported signals an unimplemented op.
var (
	ErrUnsupported = domain.Wrap(domain.CodeRAGFSError, 500,
		errors.New("operation not supported by backend"))
	ErrReadOnly = domain.Wrap(domain.CodeForbidden, 403,
		errors.New("backend is read-only"))
	ErrAlreadyExists = domain.Wrap(domain.CodeConflict, 409,
		errors.New("resource already exists"))
)

// wrapRAGFS annotates an error with the ragfs business code.
func wrapRAGFS(err error) error {
	if err == nil {
		return nil
	}
	return domain.Wrap(domain.CodeRAGFSError, 500, err)
}

// WrapRAGFS is the exported form of wrapRAGFS for use by sub-packages
// (plugins/localfs, plugins/memfs, ...). It is the canonical way for a
// backend to wrap an OS-level error before returning it to a caller.
func WrapRAGFS(err error) error { return wrapRAGFS(err) }
