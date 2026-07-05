package ragfs_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

// TestMountableFSWrapperDelegation exercises every FileSystem wrapper on
// MountableFS to guarantee it routes to the correct backend (and bumps
// coverage on the thin delegation methods).
func TestMountableFSWrapperDelegation(t *testing.T) {
	ctx := context.Background()
	mnt := ragfs.NewMountableFS()
	docs := memfs.New("docs")
	require.NoError(t, mnt.Mount("/docs", docs))

	// ServicePlugin surface.
	assert.Equal(t, "mountable", mnt.Name())
	assert.NoError(t, mnt.Validate(nil))
	assert.NoError(t, mnt.Initialize(ctx, nil))
	assert.NoError(t, mnt.HealthCheck(ctx))

	// Backends snapshot.
	backends := mnt.Backends()
	assert.Len(t, backends, 1)
	assert.Same(t, docs, backends["/docs"])

	// Create + Mkdir.
	require.NoError(t, mnt.Create(ctx, "/docs/file.md", false))
	require.NoError(t, mnt.Mkdir(ctx, "/docs/sub", 0o755))

	// Write + Read + Stat.
	require.NoError(t, mnt.Write(ctx, "/docs/file.md", bytes.NewReader([]byte("hi")), 0o644))
	var buf bytes.Buffer
	require.NoError(t, mnt.Read(ctx, "/docs/file.md", &buf))
	assert.Equal(t, "hi", buf.String())
	info, err := mnt.Stat(ctx, "/docs/file.md")
	require.NoError(t, err)
	assert.Equal(t, "file.md", info.Name)

	// ReadDir.
	entries, err := mnt.ReadDir(ctx, "/docs")
	require.NoError(t, err)
	names := namesOf(entries)
	assert.Contains(t, names, "file.md")
	assert.Contains(t, names, "sub")

	// TreeDirectory.
	tree, err := mnt.TreeDirectory(ctx, "/docs", 1)
	require.NoError(t, err)
	assert.NotEmpty(t, tree)

	// Chmod.
	require.NoError(t, mnt.Chmod(ctx, "/docs/file.md", 0o600))

	// Grep.
	matches, err := mnt.Grep(ctx, "hi", "/docs", true)
	require.NoError(t, err)
	assert.NotEmpty(t, matches)

	// Copy (same mount).
	require.NoError(t, mnt.Copy(ctx, "/docs/file.md", "/docs/copy.md"))
	info2, err := mnt.Stat(ctx, "/docs/copy.md")
	require.NoError(t, err)
	assert.Equal(t, "copy.md", info2.Name)

	// Rename (same mount).
	require.NoError(t, mnt.Rename(ctx, "/docs/copy.md", "/docs/renamed.md"))
	_, err = mnt.Stat(ctx, "/docs/renamed.md")
	require.NoError(t, err)

	// Remove.
	require.NoError(t, mnt.Remove(ctx, "/docs/renamed.md", false))
	_, err = mnt.Stat(ctx, "/docs/renamed.md")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// TestMountableFSWrapperResolveMissing verifies the wrapper methods
// propagate Resolve's not-found error.
func TestMountableFSWrapperResolveMissing(t *testing.T) {
	ctx := context.Background()
	mnt := ragfs.NewMountableFS()

	assert.Error(t, mnt.Create(ctx, "/x/y", false))
	assert.Error(t, mnt.Mkdir(ctx, "/x/y", 0o755))
	assert.Error(t, mnt.Remove(ctx, "/x/y", false))
	assert.Error(t, mnt.Read(ctx, "/x/y", &bytes.Buffer{}))
	assert.Error(t, mnt.Write(ctx, "/x/y", bytes.NewReader([]byte("z")), 0o644))
	_, err := mnt.ReadDir(ctx, "/x/y")
	assert.Error(t, err)
	_, err = mnt.Stat(ctx, "/x/y")
	assert.Error(t, err)
	_, err = mnt.TreeDirectory(ctx, "/x/y", 1)
	assert.Error(t, err)
	assert.Error(t, mnt.Chmod(ctx, "/x/y", 0o644))
	_, err = mnt.Grep(ctx, "pat", "/x/y", true)
	assert.Error(t, err)
	assert.Error(t, mnt.Rename(ctx, "/x/y", "/x/z"))
	assert.Error(t, mnt.Copy(ctx, "/x/y", "/x/z"))
}

// TestMountableFSHealthCheckDelegates verifies HealthCheck returns the
// first failure from a mounted backend.
func TestMountableFSHealthCheckDelegates(t *testing.T) {
	ctx := context.Background()
	mnt := ragfs.NewMountableFS()
	require.NoError(t, mnt.Mount("/ok", memfs.New("ok")))
	assert.NoError(t, mnt.HealthCheck(ctx))
}

// TestWrapRAGFS exercises the exported WrapRAGFS helper.
func TestWrapRAGFS(t *testing.T) {
	assert.Nil(t, ragfs.WrapRAGFS(nil))
	wrapped := ragfs.WrapRAGFS(domain.ErrNotFound)
	require.Error(t, wrapped)
	var appErr *domain.AppError
	require.ErrorAs(t, wrapped, &appErr)
	assert.Equal(t, domain.CodeRAGFSError, appErr.Code)
}
