package ragfs_test

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

// newMultiWrite builds a MultiWriteFS with one primary and two backup
// memfs backends, sync mode (so fanout completes before return).
func newMultiWrite(t *testing.T) (*ragfs.MultiWriteFS, *memfs.MemFS, *memfs.MemFS, *memfs.MemFS) {
	t.Helper()
	primary := memfs.New("primary")
	backup1 := memfs.New("backup1")
	backup2 := memfs.New("backup2")
	mw, err := ragfs.NewMultiWriteFS(ragfs.MultiWriteConfig{
		Primary: primary,
		Backups: []ragfs.FileSystem{backup1, backup2},
		Sync:    true,
	})
	require.NoError(t, err)
	return mw, primary, backup1, backup2
}

func TestMultiWriteFS_Accessors(t *testing.T) {
	mw, primary, backup1, backup2 := newMultiWrite(t)
	assert.Same(t, primary, mw.Primary())
	backups := mw.Backups()
	assert.Len(t, backups, 2)
	assert.Same(t, backup1, backups[0])
	assert.Same(t, backup2, backups[1])
	assert.Equal(t, "multiwrite", mw.Name())
}

func TestMultiWriteFS_ServicePlugin(t *testing.T) {
	mw, _, _, _ := newMultiWrite(t)
	ctx := context.Background()
	assert.NoError(t, mw.Validate(nil))
	assert.NoError(t, mw.Initialize(ctx, nil))
	assert.NoError(t, mw.HealthCheck(ctx))
}

func TestMultiWriteFS_CreateMkdirRemoveReplicate(t *testing.T) {
	ctx := context.Background()
	mw, primary, backup1, backup2 := newMultiWrite(t)

	require.NoError(t, mw.Create(ctx, "/file.md", false))
	require.NoError(t, mw.Mkdir(ctx, "/sub", 0o755))
	// All backends should have the file and dir.
	for _, fs := range []ragfs.FileSystem{primary, backup1, backup2} {
		_, err := fs.Stat(ctx, "/file.md")
		assert.NoError(t, err, "backend missing /file.md")
		_, err = fs.Stat(ctx, "/sub")
		assert.NoError(t, err, "backend missing /sub")
	}

	// Remove replicates.
	require.NoError(t, mw.Remove(ctx, "/file.md", false))
	for _, fs := range []ragfs.FileSystem{primary, backup1, backup2} {
		_, err := fs.Stat(ctx, "/file.md")
		assert.Error(t, err, "backend still has /file.md after remove")
	}
}

func TestMultiWriteFS_WriteReadStatReadDir(t *testing.T) {
	ctx := context.Background()
	mw, _, _, _ := newMultiWrite(t)

	require.NoError(t, mw.Write(ctx, "/a.md", bytes.NewReader([]byte("hello")), 0o644))
	// Read from primary.
	var buf bytes.Buffer
	require.NoError(t, mw.Read(ctx, "/a.md", &buf))
	assert.Equal(t, "hello", buf.String())

	// Stat.
	info, err := mw.Stat(ctx, "/a.md")
	require.NoError(t, err)
	assert.Equal(t, "a.md", info.Name)

	// ReadDir.
	entries, err := mw.ReadDir(ctx, "/")
	require.NoError(t, err)
	assert.NotEmpty(t, entries)

	// Grep.
	matches, err := mw.Grep(ctx, "hello", "/", true)
	require.NoError(t, err)
	assert.NotEmpty(t, matches)

	// TreeDirectory.
	tree, err := mw.TreeDirectory(ctx, "/", 1)
	require.NoError(t, err)
	assert.NotEmpty(t, tree)
}

func TestMultiWriteFS_RenameReplicate(t *testing.T) {
	ctx := context.Background()
	mw, primary, backup1, backup2 := newMultiWrite(t)

	require.NoError(t, mw.Write(ctx, "/old.md", bytes.NewReader([]byte("x")), 0o644))
	require.NoError(t, mw.Rename(ctx, "/old.md", "/new.md"))

	for _, fs := range []ragfs.FileSystem{primary, backup1, backup2} {
		_, err := fs.Stat(ctx, "/new.md")
		assert.NoError(t, err, "backend missing /new.md after rename")
	}
}

func TestMultiWriteFS_CopyReplicate(t *testing.T) {
	ctx := context.Background()
	mw, primary, backup1, backup2 := newMultiWrite(t)

	require.NoError(t, mw.Write(ctx, "/orig.md", bytes.NewReader([]byte("c")), 0o644))
	require.NoError(t, mw.Copy(ctx, "/orig.md", "/copy.md"))

	for _, fs := range []ragfs.FileSystem{primary, backup1, backup2} {
		_, err := fs.Stat(ctx, "/copy.md")
		assert.NoError(t, err, "backend missing /copy.md after copy")
	}
}

func TestMultiWriteFS_ChmodReplicate(t *testing.T) {
	ctx := context.Background()
	mw, primary, backup1, backup2 := newMultiWrite(t)

	require.NoError(t, mw.Write(ctx, "/f.md", bytes.NewReader([]byte("c")), 0o644))
	require.NoError(t, mw.Chmod(ctx, "/f.md", 0o600))

	for _, fs := range []ragfs.FileSystem{primary, backup1, backup2} {
		info, err := fs.Stat(ctx, "/f.md")
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode)
	}
}

// noCopyFS wraps a FileSystem and returns ErrUnsupported from Copy, forcing
// MultiWriteFS.copyViaRead / copyViaReadOn fallback paths.
type noCopyFS struct{ ragfs.FileSystem }

func (n *noCopyFS) Copy(ctx context.Context, srcP, dstP string) error {
	return ragfs.ErrUnsupported
}

func TestMultiWriteFS_CopyFallbackToReadAndWrite(t *testing.T) {
	ctx := context.Background()
	primary := &noCopyFS{memfs.New("primary")}
	backup := &noCopyFS{memfs.New("backup")}
	mw, err := ragfs.NewMultiWriteFS(ragfs.MultiWriteConfig{
		Primary: primary,
		Backups: []ragfs.FileSystem{backup},
		Sync:    true,
	})
	require.NoError(t, err)

	require.NoError(t, mw.Write(ctx, "/orig.md", bytes.NewReader([]byte("payload")), 0o644))
	// Copy must fall back to Read+Write on the primary when Copy returns
	// ErrUnsupported. (Fanout to backups only runs when primary.Copy
	// succeeds; the fallback path emulates Copy on primary only.)
	require.NoError(t, mw.Copy(ctx, "/orig.md", "/copy.md"))

	_, err = primary.Stat(ctx, "/copy.md")
	assert.NoError(t, err, "primary should have /copy.md after copyViaRead")
}
