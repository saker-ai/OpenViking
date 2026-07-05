package ragfs_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

func newSyncLog(t *testing.T) *ragfs.SyncLogStore {
	t.Helper()
	return ragfs.NewSyncLogStore(t.TempDir() + "/.sync_log.json")
}

func TestMultiWriteFSSyncReplication(t *testing.T) {
	ctx := context.Background()
	primary := memfs.New("primary")
	backup1 := memfs.New("backup1")
	backup2 := memfs.New("backup2")
	log := newSyncLog(t)

	mw, err := ragfs.NewMultiWriteFS(ragfs.MultiWriteConfig{
		Primary: primary,
		Backups: []ragfs.FileSystem{backup1, backup2},
		SyncLog: log,
		Sync:    true,
	})
	require.NoError(t, err)

	// write goes to primary and both backups
	require.NoError(t, mw.Write(ctx, "/a.md", bytes.NewReader([]byte("hello")), 0o644))

	for _, fs := range []ragfs.FileSystem{primary, backup1, backup2} {
		var buf bytes.Buffer
		require.NoError(t, fs.Read(ctx, "/a.md", &buf), "backend %s", fs.Name())
		assert.Equal(t, "hello", buf.String())
	}

	// sync log records SYNC entries for both backups
	entries := log.Entries()
	require.Len(t, entries, 2)
	for _, e := range entries {
		assert.Equal(t, ragfs.OpSync, e.Op)
	}
}

func TestMultiWriteFSReadFallback(t *testing.T) {
	ctx := context.Background()
	primary := memfs.New("primary")
	backup := memfs.New("backup")
	log := newSyncLog(t)

	mw, err := ragfs.NewMultiWriteFS(ragfs.MultiWriteConfig{
		Primary: primary,
		Backups: []ragfs.FileSystem{backup},
		SyncLog: log,
		Sync:    true,
	})
	require.NoError(t, err)

	// write to mw so backup has the bytes
	require.NoError(t, mw.Write(ctx, "/x.md", bytes.NewReader([]byte("backed-up")), 0o644))

	// delete from primary to simulate primary failure
	require.NoError(t, primary.Remove(ctx, "/x.md", false))

	// read should fall back to backup
	var buf bytes.Buffer
	require.NoError(t, mw.Read(ctx, "/x.md", &buf))
	assert.Equal(t, "backed-up", buf.String())
}

func TestMultiWriteFSLargeFileRedirect(t *testing.T) {
	ctx := context.Background()
	primary := memfs.New("primary")
	backup := memfs.New("backup")
	log := newSyncLog(t)

	mw, err := ragfs.NewMultiWriteFS(ragfs.MultiWriteConfig{
		Primary:  primary,
		Backups:  []ragfs.FileSystem{backup},
		SyncLog:  log,
		Sync:     true,
		Redirect: ragfs.RedirectPolicy{FileOverSize: 16},
	})
	require.NoError(t, err)

	// 32 bytes > 16-byte threshold -> redirect
	big := bytes.Repeat([]byte("a"), 32)
	require.NoError(t, mw.Write(ctx, "/big.bin", bytes.NewReader(big), 0o644))

	// the resource path should NOT contain the bytes directly; instead a
	// .redirect.json pointer should exist
	var buf bytes.Buffer
	require.NoError(t, mw.Read(ctx, "/big.bin", &buf))
	assert.Equal(t, string(big), buf.String())

	// the redirect pointer should exist on primary
	r, err := ragfs.LoadRedirect(ctx, primary, "/big.bin")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, ragfs.RedirectFileOverSize, r.Type)
	assert.Equal(t, int64(32), r.Size)
	assert.Equal(t, "/big.bin.blob", r.Target)

	// the blob should be on primary and backup
	for _, fs := range []ragfs.FileSystem{primary, backup} {
		var b bytes.Buffer
		require.NoError(t, fs.Read(ctx, "/big.bin.blob", &b), "backend %s", fs.Name())
		assert.Equal(t, string(big), b.String())
	}
}

func TestMultiWriteFSRemove(t *testing.T) {
	ctx := context.Background()
	primary := memfs.New("primary")
	backup := memfs.New("backup")
	log := newSyncLog(t)

	mw, err := ragfs.NewMultiWriteFS(ragfs.MultiWriteConfig{
		Primary: primary,
		Backups: []ragfs.FileSystem{backup},
		SyncLog: log,
		Sync:    true,
	})
	require.NoError(t, err)

	require.NoError(t, mw.Write(ctx, "/rm.md", bytes.NewReader([]byte("x")), 0o644))
	require.NoError(t, mw.Remove(ctx, "/rm.md", false))

	for _, fs := range []ragfs.FileSystem{primary, backup} {
		err := fs.Read(ctx, "/rm.md", &bytes.Buffer{})
		assert.ErrorIs(t, err, domain.ErrNotFound, "backend %s", fs.Name())
	}
}

func TestMultiWriteFSPrimaryNil(t *testing.T) {
	_, err := ragfs.NewMultiWriteFS(ragfs.MultiWriteConfig{})
	require.Error(t, err)
}

func TestRedirectPolicyShouldRedirect(t *testing.T) {
	p := ragfs.RedirectPolicy{FileOverSize: 100, FileExtensions: []string{".mp4"}}
	assert.True(t, p.ShouldRedirect(101, "/x"))
	assert.False(t, p.ShouldRedirect(100, "/x"))
	assert.True(t, p.ShouldRedirect(10, "/clip.mp4"))
	assert.False(t, p.ShouldRedirect(10, "/clip.md"))
	assert.False(t, p.ShouldRedirect(-1, "/unknown")) // unknown size
}

func TestSyncLogStoreLoad(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/.sync_log.json"
	log := ragfs.NewSyncLogStore(path)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, log.Append(ctx, ragfs.SyncLogEntry{
		Op: ragfs.OpWrite, Path: "/a", Backend: "primary", Timestamp: now,
	}))
	require.NoError(t, log.Append(ctx, ragfs.SyncLogEntry{
		Op: ragfs.OpPending, Path: "/a", Backend: "backup1", RetryCount: 1,
	}))

	// fresh store re-reads from disk
	log2 := ragfs.NewSyncLogStore(path)
	require.NoError(t, log2.Load())
	entries := log2.Entries()
	require.Len(t, entries, 2)
	assert.Equal(t, ragfs.OpWrite, entries[0].Op)
	assert.Equal(t, ragfs.OpPending, entries[1].Op)

	// pending filter
	pending := log2.Pending(3)
	require.Len(t, pending, 1)
	assert.Equal(t, "backup1", pending[0].Backend)
}

func TestSyncLogStoreBatch(t *testing.T) {
	ctx := context.Background()
	log := newSyncLog(t)
	batch := []ragfs.SyncLogEntry{
		{Op: ragfs.OpWrite, Path: "/x", Backend: "b1"},
		{Op: ragfs.OpWrite, Path: "/x", Backend: "b2"},
		{Op: ragfs.OpSync, Path: "/x", Backend: "b1"},
	}
	require.NoError(t, log.AppendBatch(ctx, batch))
	assert.Len(t, log.Entries(), 3)
}
