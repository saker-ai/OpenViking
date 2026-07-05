package ragfs_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

func TestHasOverview(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("docs")
	require.NoError(t, ragfs.WriteOverview(ctx, fs, "/doc.md", "overview text"))

	has, err := ragfs.HasOverview(ctx, fs, "/doc.md")
	require.NoError(t, err)
	assert.True(t, has)

	has, err = ragfs.HasOverview(ctx, fs, "/missing.md")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestLoadSyncLog_Missing(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("docs")
	entries, err := ragfs.LoadSyncLog(ctx, fs, "/doc.md")
	require.NoError(t, err)
	assert.Nil(t, entries)
}

func TestWriteAndLoadSyncLog_RoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("docs")
	want := []ragfs.SyncLogEntry{
		{Op: ragfs.OpWrite, Path: "/a.md"},
		{Op: ragfs.OpDelete, Path: "/c.md"},
	}
	require.NoError(t, ragfs.WriteSyncLog(ctx, fs, "/doc.md", want))

	got, err := ragfs.LoadSyncLog(ctx, fs, "/doc.md")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, ragfs.OpWrite, got[0].Op)
	assert.Equal(t, "/a.md", got[0].Path)
	assert.Equal(t, ragfs.OpDelete, got[1].Op)
}

func TestSyncLogStore_LoadFlushRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sync.json")
	store := ragfs.NewSyncLogStore(path)

	require.NoError(t, store.Append(ctx, ragfs.SyncLogEntry{Op: ragfs.OpWrite, Path: "/x"}))
	require.NoError(t, store.Append(ctx, ragfs.SyncLogEntry{Op: ragfs.OpDelete, Path: "/y"}))
	require.Len(t, store.Entries(), 2)

	// Re-open a fresh store over the same path; Load should restore entries.
	store2 := ragfs.NewSyncLogStore(path)
	require.NoError(t, store2.Load())
	require.Len(t, store2.Entries(), 2)
}

func TestMultiWriteFS_AsyncFanout(t *testing.T) {
	ctx := context.Background()
	primary := memfs.New("primary")
	backup := memfs.New("backup")
	mw, err := ragfs.NewMultiWriteFS(ragfs.MultiWriteConfig{
		Primary:     primary,
		Backups:     []ragfs.FileSystem{backup},
		Sync:        false,
		AsyncFanout: true,
	})
	require.NoError(t, err)

	require.NoError(t, mw.Write(ctx, "/async.md", bytes.NewReader([]byte("payload")), 0o644))
	// In async mode, fanout happens in a goroutine. Wait for the backup
	// to receive the write by polling briefly.
	require.Eventually(t, func() bool {
		_, err := backup.Stat(ctx, "/async.md")
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
}
