// Package memfs implements the ragfs FileSystem interface backed by an
// in-memory map. It is intended for tests and ephemeral mounts where
// persistence is not required.
//
// It corresponds to crates/ragfs/src/plugins/memfs/ in the Rust
// implementation. All operations are safe for concurrent use.
package memfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
)

// MemFS is an in-memory ragfs backend. The zero value is NOT usable; use
// New.
type MemFS struct {
	mu   sync.RWMutex
	root *node
	name string
}

// node is one entry in the in-memory tree. A node is either a directory
// (children != nil) or a file (data != nil).
type node struct {
	name     string
	isDir    bool
	mode     os.FileMode
	modTime  time.Time
	data     []byte
	children map[string]*node
}

// New returns an empty in-memory filesystem.
func New(name string) *MemFS {
	return &MemFS{
		name: name,
		root: &node{
			name:     "/",
			isDir:    true,
			mode:     0o755,
			modTime:  time.Now().UTC(),
			children: make(map[string]*node),
		},
	}
}

// --- ServicePlugin ---

func (m *MemFS) Name() string {
	if m.name == "" {
		return "memfs"
	}
	return m.name
}

func (m *MemFS) Validate(*ragfs.PluginConfig) error { return nil }

func (m *MemFS) Initialize(context.Context, *ragfs.PluginConfig) error { return nil }

func (m *MemFS) HealthCheck(context.Context) error { return nil }

// --- FileSystem ---

func (m *MemFS) Create(ctx context.Context, p string, isDir bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createLocked(p, isDir, 0o644)
}

func (m *MemFS) Mkdir(ctx context.Context, p string, perm os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createLocked(p, true, perm)
}

func (m *MemFS) Remove(ctx context.Context, p string, recursive bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	parent, base, pn, err := m.parentLocked(p)
	if err != nil {
		return err
	}
	child, ok := pn.children[base]
	if !ok {
		return domain.ErrNotFound
	}
	if child.isDir && len(child.children) > 0 && !recursive {
		return ragfs.WrapRAGFS(errString("directory not empty"))
	}
	delete(pn.children, base)
	_ = parent
	return nil
}

func (m *MemFS) Read(ctx context.Context, p string, w io.Writer) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, err := m.lookupLocked(p)
	if err != nil {
		return err
	}
	if n.isDir {
		return ragfs.WrapRAGFS(errString("read: path is a directory"))
	}
	if _, err := w.Write(n.data); err != nil {
		return ragfs.WrapRAGFS(err)
	}
	return nil
}

func (m *MemFS) Write(ctx context.Context, p string, r io.Reader, perm os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, base, pn, err := m.parentLocked(p)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return ragfs.WrapRAGFS(err)
	}
	existing, ok := pn.children[base]
	if ok && existing.isDir {
		return domain.ErrConflict
	}
	if perm == 0 {
		perm = 0o644
	}
	pn.children[base] = &node{
		name:    base,
		isDir:   false,
		mode:    perm,
		modTime: time.Now().UTC(),
		data:    buf.Bytes(),
	}
	return nil
}

func (m *MemFS) ReadDir(ctx context.Context, p string) ([]*ragfs.TreeEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, err := m.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	if !n.isDir {
		return nil, ragfs.WrapRAGFS(errString("readdir: not a directory"))
	}
	keys := make([]string, 0, len(n.children))
	for k := range n.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*ragfs.TreeEntry, 0, len(keys))
	for _, k := range keys {
		child := n.children[k]
		fi := &ragfs.FileInfo{
			Name:    child.name,
			Size:    int64(len(child.data)),
			Mode:    child.mode,
			ModTime: child.modTime,
			IsDir:   child.isDir,
		}
		out = append(out, &ragfs.TreeEntry{
			Path:    path.Join(p, k),
			RelPath: path.Join(p, k),
			Info:    fi,
		})
	}
	return out, nil
}

func (m *MemFS) Stat(ctx context.Context, p string) (*ragfs.FileInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, err := m.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	return &ragfs.FileInfo{
		Name:    n.name,
		Size:    int64(len(n.data)),
		Mode:    n.mode,
		ModTime: n.modTime,
		IsDir:   n.isDir,
	}, nil
}

func (m *MemFS) Rename(ctx context.Context, oldP, newP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, oldBase, oldParent, err := m.parentLocked(oldP)
	if err != nil {
		return err
	}
	child, ok := oldParent.children[oldBase]
	if !ok {
		return domain.ErrNotFound
	}
	_, newBase, newParent, err := m.parentLocked(newP)
	if err != nil {
		return err
	}
	if _, exists := newParent.children[newBase]; exists {
		return domain.ErrConflict
	}
	delete(oldParent.children, oldBase)
	child.name = newBase
	newParent.children[newBase] = child
	return nil
}

func (m *MemFS) Copy(ctx context.Context, srcP, dstP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	src, err := m.lookupLocked(srcP)
	if err != nil {
		return err
	}
	if src.isDir {
		return ragfs.ErrUnsupported
	}
	_, dstBase, dstParent, err := m.parentLocked(dstP)
	if err != nil {
		return err
	}
	if _, exists := dstParent.children[dstBase]; exists && dstParent.children[dstBase].isDir {
		return domain.ErrConflict
	}
	data := make([]byte, len(src.data))
	copy(data, src.data)
	dstParent.children[dstBase] = &node{
		name:    dstBase,
		isDir:   false,
		mode:    src.mode,
		modTime: time.Now().UTC(),
		data:    data,
	}
	return nil
}

func (m *MemFS) Chmod(ctx context.Context, p string, perm os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookupLocked(p)
	if err != nil {
		return err
	}
	n.mode = perm
	n.modTime = time.Now().UTC()
	return nil
}

func (m *MemFS) Grep(ctx context.Context, pattern, p string, recursive bool) ([]ragfs.GrepMatch, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, err := m.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	var out []ragfs.GrepMatch
	if n.isDir {
		walkNode(n, p, func(nodePath string, child *node) {
			if child.isDir {
				return
			}
			out = append(out, grepBytes(pattern, nodePath, child.data)...)
		})
		_ = recursive
	} else {
		out = append(out, grepBytes(pattern, p, n.data)...)
	}
	return out, nil
}

func (m *MemFS) TreeDirectory(ctx context.Context, p string, depth int) ([]*ragfs.TreeEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, err := m.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	if !n.isDir {
		return nil, ragfs.WrapRAGFS(errString("tree: not a directory"))
	}
	if depth <= 0 {
		depth = 1
	}
	var out []*ragfs.TreeEntry
	collectTree(n, p, depth, 1, &out)
	return out, nil
}

// --- internals ---

// createLocked is shared by Create and Mkdir.
func (m *MemFS) createLocked(p string, isDir bool, perm os.FileMode) error {
	_, base, pn, err := m.parentLocked(p)
	if err != nil {
		return err
	}
	if _, exists := pn.children[base]; exists {
		return domain.ErrConflict
	}
	if perm == 0 {
		if isDir {
			perm = 0o755
		} else {
			perm = 0o644
		}
	}
	pn.children[base] = &node{
		name:     base,
		isDir:    isDir,
		mode:     perm,
		modTime:  time.Now().UTC(),
		children: mapIf(isDir, make(map[string]*node)),
	}
	return nil
}

// parentLocked returns the parent directory node, the base name, and the
// parent path node for p. It creates intermediate directories as needed
// for Write but NOT for Create (Create uses O_EXCL semantics: parent must
// exist). To keep semantics simple we MkdirAll the parent here, matching
// localfs behavior.
func (m *MemFS) parentLocked(p string) (string, string, *node, error) {
	n := ragfs.Normalize(p)
	if n == "/" {
		return "", "/", m.root, nil
	}
	parts := strings.Split(strings.TrimPrefix(n, "/"), "/")
	cur := m.root
	for i := 0; i < len(parts)-1; i++ {
		seg := parts[i]
		if seg == "" {
			continue
		}
		child, ok := cur.children[seg]
		if !ok {
			// MkdirAll-style: create missing parent dir
			child = &node{
				name:     seg,
				isDir:    true,
				mode:     0o755,
				modTime:  time.Now().UTC(),
				children: make(map[string]*node),
			}
			cur.children[seg] = child
		} else if !child.isDir {
			return "", "", nil, ragfs.WrapRAGFS(errString("parent path is not a directory: " + seg))
		}
		cur = child
	}
	base := parts[len(parts)-1]
	if base == "" {
		return "", "/", m.root, nil
	}
	return n, base, cur, nil
}

// lookupLocked walks the tree to find p. Returns domain.ErrNotFound on miss.
func (m *MemFS) lookupLocked(p string) (*node, error) {
	n := ragfs.Normalize(p)
	if n == "/" {
		return m.root, nil
	}
	parts := strings.Split(strings.TrimPrefix(n, "/"), "/")
	cur := m.root
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		child, ok := cur.children[seg]
		if !ok {
			return nil, domain.ErrNotFound
		}
		cur = child
	}
	return cur, nil
}

// walkNode visits every descendant of n (non-recursive).
func walkNode(n *node, nodePath string, visit func(string, *node)) {
	for k, child := range n.children {
		childPath := path.Join(nodePath, k)
		visit(childPath, child)
		if child.isDir {
			walkNode(child, childPath, visit)
		}
	}
}

// collectTree walks up to `depth` levels under n.
func collectTree(n *node, nodePath string, depth, current int, out *[]*ragfs.TreeEntry) {
	for k, child := range n.children {
		childPath := path.Join(nodePath, k)
		*out = append(*out, &ragfs.TreeEntry{
			Path:    childPath,
			RelPath: childPath,
			Info: &ragfs.FileInfo{
				Name:    child.name,
				Size:    int64(len(child.data)),
				Mode:    child.mode,
				ModTime: child.modTime,
				IsDir:   child.isDir,
			},
		})
		if child.isDir && current < depth {
			collectTree(child, childPath, depth, current+1, out)
		}
	}
}

// grepBytes scans a byte slice line-by-line for pattern (literal match).
func grepBytes(pattern, p string, data []byte) []ragfs.GrepMatch {
	var out []ragfs.GrepMatch
	ln := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		ln++
		if bytes.Contains(line, []byte(pattern)) {
			out = append(out, ragfs.GrepMatch{
				Path:    p,
				LineNo:  ln,
				Line:    string(line),
				Pattern: pattern,
			})
		}
	}
	return out
}

// mapIf returns the given map when cond is true, else nil.
func mapIf(cond bool, m map[string]*node) map[string]*node {
	if cond {
		return m
	}
	return nil
}

// errString is a tiny string-as-error helper to avoid importing errors
// repeatedly in this file.
type errString string

func (e errString) Error() string { return string(e) }
