package ragfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/armon/go-radix"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// MountableFS routes FileSystem operations to backends by URI prefix.
//
// It mirrors the Rust core/mountable.rs MountableFS: a radix trie holds
// the longest-prefix match for each mounted backend, so lookups are O(k)
// in the path length. The zero value is NOT usable; use NewMountableFS.
//
// Mount prefixes are normalized POSIX-style paths (e.g. "/docs",
// "/sessions"). A mount at "/" acts as the catch-all. Internally prefixes
// are stored with a trailing slash so that "/docs" does not match
// "/docsessions/x"; the root mount is stored as "/". Paths that do not
// match any mount return domain.ErrNotFound.
type MountableFS struct {
	mu       sync.RWMutex
	trie     *radix.Tree
	backends map[string]FileSystem // keyed by user-facing prefix (no trailing slash)
}

// NewMountableFS constructs an empty mount table.
func NewMountableFS() *MountableFS {
	return &MountableFS{
		trie:     radix.New(),
		backends: make(map[string]FileSystem),
	}
}

// Mount registers fs at the given prefix. The prefix is normalized and
// must be unique; remounting an existing prefix returns a conflict error.
// A mount at "/" is the catch-all.
func (m *MountableFS) Mount(prefix string, fs FileSystem) error {
	if fs == nil {
		return wrapRAGFS(errors.New("mount: backend is nil"))
	}
	userKey := normalizePrefixUser(prefix)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.backends[userKey]; exists {
		return fmt.Errorf("mount: prefix %s already mounted: %w",
			userKey, domain.ErrConflict)
	}
	m.backends[userKey] = fs
	m.trie.Insert(trieKey(userKey), fs)
	return nil
}

// Unmount removes the backend at the given prefix. Returns
// domain.ErrNotFound when no mount exists.
func (m *MountableFS) Unmount(prefix string) error {
	userKey := normalizePrefixUser(prefix)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.backends[userKey]; !exists {
		return domain.ErrNotFound
	}
	delete(m.backends, userKey)
	m.trie.Delete(trieKey(userKey))
	return nil
}

// Resolve returns the backend and relative path for a resource path.
// Returns domain.ErrNotFound when no mount matches.
func (m *MountableFS) Resolve(p string) (FileSystem, string, error) {
	search := trieSearchKey(p)
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, raw, ok := m.trie.LongestPrefix(search)
	if !ok {
		return nil, "", domain.ErrNotFound
	}
	fs, _ := raw.(FileSystem)
	if fs == nil {
		return nil, "", domain.ErrNotFound
	}
	// relative path = path with the mount prefix stripped
	rel := relativize(p, longestUserPrefixLocked(m.backends, p))
	return fs, rel, nil
}

// Backends returns a snapshot of mounted backends keyed by prefix.
func (m *MountableFS) Backends() map[string]FileSystem {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]FileSystem, len(m.backends))
	for k, v := range m.backends {
		out[k] = v
	}
	return out
}

// --- FileSystem interface ---

// Name implements ServicePlugin.
func (m *MountableFS) Name() string { return "mountable" }

// Validate implements ServicePlugin. MountableFS is a router, not a
// configured backend; validation is a no-op.
func (m *MountableFS) Validate(*PluginConfig) error { return nil }

// Initialize implements ServicePlugin.
func (m *MountableFS) Initialize(context.Context, *PluginConfig) error { return nil }

// HealthCheck delegates to every mounted backend. Returns the first
// failure encountered.
func (m *MountableFS) HealthCheck(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, fs := range m.backends {
		if err := fs.HealthCheck(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (m *MountableFS) Create(ctx context.Context, p string, isDir bool) error {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return err
	}
	return fs.Create(ctx, rel, isDir)
}

func (m *MountableFS) Mkdir(ctx context.Context, p string, perm os.FileMode) error {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return err
	}
	return fs.Mkdir(ctx, rel, perm)
}

func (m *MountableFS) Remove(ctx context.Context, p string, recursive bool) error {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return err
	}
	return fs.Remove(ctx, rel, recursive)
}

func (m *MountableFS) Read(ctx context.Context, p string, w io.Writer) error {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return err
	}
	return fs.Read(ctx, rel, w)
}

func (m *MountableFS) Write(ctx context.Context, p string, r io.Reader, perm os.FileMode) error {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return err
	}
	return fs.Write(ctx, rel, r, perm)
}

func (m *MountableFS) ReadDir(ctx context.Context, p string) ([]*TreeEntry, error) {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return nil, err
	}
	return fs.ReadDir(ctx, rel)
}

func (m *MountableFS) Stat(ctx context.Context, p string) (*FileInfo, error) {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return nil, err
	}
	return fs.Stat(ctx, rel)
}

func (m *MountableFS) Rename(ctx context.Context, oldP, newP string) error {
	srcFS, srcRel, err := m.Resolve(oldP)
	if err != nil {
		return err
	}
	dstFS, dstRel, err := m.Resolve(newP)
	if err != nil {
		return err
	}
	if srcFS != dstFS {
		return wrapRAGFS(errors.New("mountable: rename crosses mount boundaries"))
	}
	return srcFS.Rename(ctx, srcRel, dstRel)
}

func (m *MountableFS) Copy(ctx context.Context, srcP, dstP string) error {
	srcFS, srcRel, err := m.Resolve(srcP)
	if err != nil {
		return err
	}
	dstFS, dstRel, err := m.Resolve(dstP)
	if err != nil {
		return err
	}
	if srcFS != dstFS {
		return wrapRAGFS(errors.New("mountable: copy crosses mount boundaries"))
	}
	return srcFS.Copy(ctx, srcRel, dstRel)
}

func (m *MountableFS) Chmod(ctx context.Context, p string, perm os.FileMode) error {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return err
	}
	return fs.Chmod(ctx, rel, perm)
}

func (m *MountableFS) Grep(ctx context.Context, pattern, p string, recursive bool) ([]GrepMatch, error) {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return nil, err
	}
	return fs.Grep(ctx, pattern, rel, recursive)
}

func (m *MountableFS) TreeDirectory(ctx context.Context, p string, depth int) ([]*TreeEntry, error) {
	fs, rel, err := m.Resolve(p)
	if err != nil {
		return nil, err
	}
	return fs.TreeDirectory(ctx, rel, depth)
}

// normalizePrefixUser returns the user-facing mount key (no trailing
// slash except for root). e.g. "/docs/" -> "/docs", "" -> "/", "/docs" -> "/docs".
func normalizePrefixUser(prefix string) string {
	if prefix == "" {
		return "/"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	clean := strings.TrimRight(prefix, "/")
	if clean == "" {
		return "/"
	}
	return clean
}

// trieKey returns the radix-trie key for a user-facing mount prefix.
// Non-root prefixes get a trailing slash so that "/docs/" does not match
// "/docsessions/x" (the radix trie does literal byte-prefix matching).
func trieKey(userKey string) string {
	if userKey == "/" {
		return "/"
	}
	return userKey + "/"
}

// trieSearchKey normalizes a lookup path for radix search. It always ends
// with "/" so that searching for "/docs" (the mount root) matches the
// trie key "/docs/".
func trieSearchKey(p string) string {
	n := Normalize(p)
	if !strings.HasSuffix(n, "/") {
		return n + "/"
	}
	return n
}

// longestUserPrefixLocked returns the user-facing mount prefix that is the
// longest path-component prefix of p. Caller holds m.mu or m.mu.RLock.
func longestUserPrefixLocked(backends map[string]FileSystem, p string) string {
	n := Normalize(p)
	best := ""
	for k := range backends {
		if k == "/" {
			if best == "" {
				best = "/"
			}
			continue
		}
		if n == k || strings.HasPrefix(n, k+"/") {
			if len(k) > len(best) {
				best = k
			}
		}
	}
	return best
}

// relativize strips the mount prefix from a path, yielding the path
// relative to the backend. The result always begins with "/".
func relativize(p, mountPrefix string) string {
	n := Normalize(p)
	if mountPrefix == "" || mountPrefix == "/" {
		return n
	}
	rel := strings.TrimPrefix(n, mountPrefix)
	if rel == "" {
		return "/"
	}
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	return rel
}
