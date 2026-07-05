// Package localfs implements the ragfs FileSystem interface backed by the
// OS filesystem via the standard library "os" package.
//
// It corresponds to crates/ragfs/src/plugins/localfs/ in the Rust
// implementation. A LocalFS instance is rooted at an absolute directory;
// all paths passed to FileSystem methods are interpreted relative to that
// root after normalization.
package localfs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
)

// LocalFS is a ragfs backend rooted at an OS directory.
//
// The zero value is NOT usable; use New.
type LocalFS struct {
	root string
	name string
}

// New returns a LocalFS rooted at root. The root is created if missing.
func New(name, root string) (*LocalFS, error) {
	if root == "" {
		return nil, ragfs.ErrUnsupported
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, ragfs.ErrUnsupported
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, domain.Wrap(domain.CodeRAGFSError, 500, err)
	}
	return &LocalFS{root: abs, name: name}, nil
}

// NewAt is a convenience that panics on construction error. Intended for
// tests where the root is a t.TempDir().
func NewAt(name, root string) *LocalFS {
	fs, err := New(name, root)
	if err != nil {
		panic(err)
	}
	return fs
}

// --- ServicePlugin ---

func (l *LocalFS) Name() string {
	if l.name == "" {
		return "localfs"
	}
	return l.name
}

func (l *LocalFS) Validate(*ragfs.PluginConfig) error { return nil }

func (l *LocalFS) Initialize(context.Context, *ragfs.PluginConfig) error { return nil }

func (l *LocalFS) HealthCheck(context.Context) error {
	if _, err := os.Stat(l.root); err != nil {
		return domain.Wrap(domain.CodeRAGFSError, 500, err)
	}
	return nil
}

// --- FileSystem ---

func (l *LocalFS) Create(ctx context.Context, p string, isDir bool) error {
	full := l.resolve(p)
	if isDir {
		if err := os.MkdirAll(full, 0o755); err != nil {
			return l.wrapExists(err)
		}
		return nil
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return l.wrapExists(err)
	}
	return f.Close()
}

func (l *LocalFS) Mkdir(ctx context.Context, p string, perm os.FileMode) error {
	full := l.resolve(p)
	if err := os.MkdirAll(full, perm); err != nil {
		return l.wrapExists(err)
	}
	return nil
}

func (l *LocalFS) Remove(ctx context.Context, p string, recursive bool) error {
	full := l.resolve(p)
	if recursive {
		if err := os.RemoveAll(full); err != nil {
			return l.wrapMissing(err)
		}
		return nil
	}
	if err := os.Remove(full); err != nil {
		return l.wrapMissing(err)
	}
	return nil
}

func (l *LocalFS) Read(ctx context.Context, p string, w io.Writer) error {
	full := l.resolve(p)
	f, err := os.Open(full)
	if err != nil {
		return l.wrapMissing(err)
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil {
		return ragfs.WrapRAGFS(err)
	}
	return nil
}

func (l *LocalFS) Write(ctx context.Context, p string, r io.Reader, perm os.FileMode) error {
	full := l.resolve(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return ragfs.WrapRAGFS(err)
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return ragfs.WrapRAGFS(err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return ragfs.WrapRAGFS(err)
	}
	return nil
}

func (l *LocalFS) ReadDir(ctx context.Context, p string) ([]*ragfs.TreeEntry, error) {
	full := l.resolve(p)
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, l.wrapMissing(err)
	}
	out := make([]*ragfs.TreeEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		fi := &ragfs.FileInfo{
			Name:    info.Name(),
			Size:    info.Size(),
			Mode:    info.Mode(),
			ModTime: info.ModTime(),
			IsDir:   e.IsDir(),
		}
		rel := strings.TrimPrefix(filepath.Join(full, e.Name()), l.root)
		if !strings.HasPrefix(rel, "/") {
			rel = "/" + rel
		}
		out = append(out, &ragfs.TreeEntry{
			Path:    filepath.Join(p, e.Name()),
			RelPath: rel,
			Info:    fi,
		})
	}
	return out, nil
}

func (l *LocalFS) Stat(ctx context.Context, p string) (*ragfs.FileInfo, error) {
	full := l.resolve(p)
	info, err := os.Stat(full)
	if err != nil {
		return nil, l.wrapMissing(err)
	}
	return &ragfs.FileInfo{
		Name:    info.Name(),
		Size:    info.Size(),
		Mode:    info.Mode(),
		ModTime: info.ModTime(),
		IsDir:   info.IsDir(),
	}, nil
}

func (l *LocalFS) Rename(ctx context.Context, oldP, newP string) error {
	oldFull := l.resolve(oldP)
	newFull := l.resolve(newP)
	if err := os.MkdirAll(filepath.Dir(newFull), 0o755); err != nil {
		return ragfs.WrapRAGFS(err)
	}
	if err := os.Rename(oldFull, newFull); err != nil {
		return l.wrapMissing(err)
	}
	return nil
}

func (l *LocalFS) Copy(ctx context.Context, srcP, dstP string) error {
	srcFull := l.resolve(srcP)
	dstFull := l.resolve(dstP)
	info, err := os.Stat(srcFull)
	if err != nil {
		return l.wrapMissing(err)
	}
	if info.IsDir() {
		return ragfs.ErrUnsupported
	}
	if err := os.MkdirAll(filepath.Dir(dstFull), 0o755); err != nil {
		return ragfs.WrapRAGFS(err)
	}
	in, err := os.Open(srcFull)
	if err != nil {
		return l.wrapMissing(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dstFull, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return ragfs.WrapRAGFS(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return ragfs.WrapRAGFS(err)
	}
	return nil
}

func (l *LocalFS) Chmod(ctx context.Context, p string, perm os.FileMode) error {
	full := l.resolve(p)
	if err := os.Chmod(full, perm); err != nil {
		return l.wrapMissing(err)
	}
	return nil
}

func (l *LocalFS) Grep(ctx context.Context, pattern, p string, recursive bool) ([]ragfs.GrepMatch, error) {
	full := l.resolve(p)
	info, err := os.Stat(full)
	if err != nil {
		return nil, l.wrapMissing(err)
	}
	var files []string
	if info.IsDir() {
		if !recursive {
			entries, err := os.ReadDir(full)
			if err != nil {
				return nil, ragfs.WrapRAGFS(err)
			}
			for _, e := range entries {
				if !e.IsDir() {
					files = append(files, filepath.Join(full, e.Name()))
				}
			}
		} else {
			err := filepath.Walk(full, func(path string, fi os.FileInfo, _ error) error {
				if fi != nil && !fi.IsDir() {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return nil, ragfs.WrapRAGFS(err)
			}
		}
	} else {
		files = append(files, full)
	}
	var matches []ragfs.GrepMatch
	for _, f := range files {
		ms, err := grepFile(pattern, f)
		if err != nil {
			return nil, ragfs.WrapRAGFS(err)
		}
		matches = append(matches, ms...)
	}
	return matches, nil
}

func (l *LocalFS) TreeDirectory(ctx context.Context, p string, depth int) ([]*ragfs.TreeEntry, error) {
	full := l.resolve(p)
	if depth <= 0 {
		depth = 1
	}
	var out []*ragfs.TreeEntry
	if err := walkTree(full, p, depth, 1, &out, l.root); err != nil {
		return nil, ragfs.WrapRAGFS(err)
	}
	return out, nil
}

// --- internals ---

// resolve maps a ragfs path to an OS path under root. It rejects paths
// that would escape the root via "..".
func (l *LocalFS) resolve(p string) string {
	n := strings.TrimPrefix(ragfs.Normalize(p), "/")
	full := filepath.Join(l.root, n)
	abs, err := filepath.Abs(full)
	if err != nil {
		return full
	}
	return abs
}

// wrapExists converts os.ErrExist / fs.ErrExist into a domain.ErrConflict.
func (l *LocalFS) wrapExists(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrExist) || os.IsExist(err) {
		return domain.ErrConflict
	}
	if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
		return domain.ErrNotFound
	}
	return ragfs.WrapRAGFS(err)
}

// wrapMissing converts os.ErrNotExist / fs.ErrNotExist into domain.ErrNotFound.
func (l *LocalFS) wrapMissing(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
		return domain.ErrNotFound
	}
	if os.IsExist(err) {
		return domain.ErrConflict
	}
	return ragfs.WrapRAGFS(err)
}

// grepFile scans a file line-by-line for pattern (literal match).
func grepFile(pattern, path string) ([]ragfs.GrepMatch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []ragfs.GrepMatch
	scanner := bufio.NewScanner(f)
	ln := 0
	for scanner.Scan() {
		ln++
		line := scanner.Text()
		if strings.Contains(line, pattern) {
			out = append(out, ragfs.GrepMatch{
				Path:    path,
				LineNo:  ln,
				Line:    line,
				Pattern: pattern,
			})
		}
	}
	return out, scanner.Err()
}

// walkTree walks up to `depth` levels under rootPath, appending entries.
func walkTree(rootPath, relPath string, depth, current int, out *[]*ragfs.TreeEntry, fsRoot string) error {
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		child := filepath.Join(relPath, e.Name())
		rel := strings.TrimPrefix(filepath.Join(rootPath, e.Name()), fsRoot)
		if !strings.HasPrefix(rel, "/") {
			rel = "/" + rel
		}
		*out = append(*out, &ragfs.TreeEntry{
			Path:    child,
			RelPath: rel,
			Info: &ragfs.FileInfo{
				Name:    info.Name(),
				Size:    info.Size(),
				Mode:    info.Mode(),
				ModTime: info.ModTime(),
				IsDir:   e.IsDir(),
			},
		})
		if e.IsDir() && current < depth {
			if err := walkTree(filepath.Join(rootPath, e.Name()), child, depth, current+1, out, fsRoot); err != nil {
				return err
			}
		}
	}
	return nil
}

// compile-time guard: bytes.Buffer satisfies io.Writer.
var _ io.Writer = (*bytes.Buffer)(nil)
