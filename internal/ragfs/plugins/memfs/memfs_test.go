package memfs_test

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

func newMemFS() *memfs.MemFS { return memfs.New("test") }

func TestMemFSCRUD(t *testing.T) {
	ctx := context.Background()
	fs := newMemFS()

	// Write a file (intermediate dirs auto-created).
	require.NoError(t, fs.Write(ctx, "/a/b/c.md", bytes.NewReader([]byte("hello")), 0o644))

	// Read it back.
	var buf bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/a/b/c.md", &buf))
	assert.Equal(t, "hello", buf.String())

	// Stat it.
	fi, err := fs.Stat(ctx, "/a/b/c.md")
	require.NoError(t, err)
	assert.False(t, fi.IsDir)
	assert.Equal(t, int64(5), fi.Size)
	assert.Equal(t, "c.md", fi.Name)

	// ReadDir on /a/b.
	entries, err := fs.ReadDir(ctx, "/a/b")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "c.md", entries[0].Info.Name)

	// Rename.
	require.NoError(t, fs.Rename(ctx, "/a/b/c.md", "/a/b/d.md"))
	_, err = fs.Stat(ctx, "/a/b/c.md")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	fi, err = fs.Stat(ctx, "/a/b/d.md")
	require.NoError(t, err)
	assert.Equal(t, "d.md", fi.Name)

	// Copy.
	require.NoError(t, fs.Copy(ctx, "/a/b/d.md", "/a/b/e.md"))
	var buf2 bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/a/b/e.md", &buf2))
	assert.Equal(t, "hello", buf2.String())

	// Remove.
	require.NoError(t, fs.Remove(ctx, "/a/b/d.md", false))
	require.NoError(t, fs.Remove(ctx, "/a/b/e.md", false))
	_, err = fs.Stat(ctx, "/a/b/d.md")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMemFSMkdir(t *testing.T) {
	ctx := context.Background()
	fs := newMemFS()
	require.NoError(t, fs.Mkdir(ctx, "/x/y/z", 0o755))
	fi, err := fs.Stat(ctx, "/x/y/z")
	require.NoError(t, err)
	assert.True(t, fi.IsDir)
}

func TestMemFSRemoveRecursive(t *testing.T) {
	ctx := context.Background()
	fs := newMemFS()
	require.NoError(t, fs.Write(ctx, "/dir/a.md", bytes.NewReader([]byte("a")), 0o644))
	require.NoError(t, fs.Write(ctx, "/dir/sub/b.md", bytes.NewReader([]byte("b")), 0o644))

	// non-recursive remove on non-empty dir fails
	err := fs.Remove(ctx, "/dir", false)
	assert.Error(t, err)

	// recursive remove works
	require.NoError(t, fs.Remove(ctx, "/dir", true))
	_, err = fs.Stat(ctx, "/dir")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMemFSChmod(t *testing.T) {
	ctx := context.Background()
	fs := newMemFS()
	require.NoError(t, fs.Write(ctx, "/f.md", bytes.NewReader([]byte("x")), 0o644))
	require.NoError(t, fs.Chmod(ctx, "/f.md", 0o600))
	fi, err := fs.Stat(ctx, "/f.md")
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode.Perm())
}

func TestMemFSReadMissing(t *testing.T) {
	ctx := context.Background()
	fs := newMemFS()
	err := fs.Read(ctx, "/nope.md", &bytes.Buffer{})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMemFSReadDirOnFileFails(t *testing.T) {
	ctx := context.Background()
	fs := newMemFS()
	require.NoError(t, fs.Write(ctx, "/f.md", bytes.NewReader([]byte("x")), 0o644))
	_, err := fs.ReadDir(ctx, "/f.md")
	assert.Error(t, err)
}

func TestMemFSReadOnDirFails(t *testing.T) {
	ctx := context.Background()
	fs := newMemFS()
	require.NoError(t, fs.Mkdir(ctx, "/d", 0o755))
	err := fs.Read(ctx, "/d", &bytes.Buffer{})
	assert.Error(t, err)
}

func TestMemFSGrep(t *testing.T) {
	ctx := context.Background()
	fs := newMemFS()
	require.NoError(t, fs.Write(ctx, "/a.md", bytes.NewReader([]byte("foo\nbar\n")), 0o644))
	require.NoError(t, fs.Write(ctx, "/b.md", bytes.NewReader([]byte("foo here\n")), 0o644))
	matches, err := fs.Grep(ctx, "foo", "/", true)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(matches), 2)
}

func TestMemFSTreeDirectory(t *testing.T) {
	ctx := context.Background()
	fs := newMemFS()
	require.NoError(t, fs.Mkdir(ctx, "/a", 0o755))
	require.NoError(t, fs.Write(ctx, "/a/1.md", bytes.NewReader([]byte("1")), 0o644))
	require.NoError(t, fs.Write(ctx, "/a/2.md", bytes.NewReader([]byte("2")), 0o644))
	require.NoError(t, fs.Mkdir(ctx, "/a/sub", 0o755))
	require.NoError(t, fs.Write(ctx, "/a/sub/3.md", bytes.NewReader([]byte("3")), 0o644))

	entries, err := fs.TreeDirectory(ctx, "/a", 2)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(entries), 3)
}
