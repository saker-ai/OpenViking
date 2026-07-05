// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package server

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func newLocalTempStore(t *testing.T) *TempUploadStore {
	t.Helper()
	s, err := NewTempUploadStore(config.TempUploadConfig{Mode: "local", MaxBytes: 1024})
	require.NoError(t, err)
	return s
}

func newSharedTempStore(t *testing.T) *TempUploadStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewTempUploadStore(config.TempUploadConfig{Mode: "shared", SharedDir: dir, MaxBytes: 1024})
	require.NoError(t, err)
	return s
}

func TestTempUploadStore_CreateAppendFinalize_Local(t *testing.T) {
	s := newLocalTempStore(t)
	ctx := context.Background()

	f, err := s.Create(ctx, "acct", "report.pdf", 1024)
	require.NoError(t, err)
	require.NotNil(t, f)
	// Don't close f — Append writes through the store's file handle and
	// Finalize is responsible for closing it. Abort is the cleanup if
	// the test bails early.
	metas, err := s.List(ctx, "acct")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	uploadID := metas[0].ID
	t.Cleanup(func() { _ = s.Abort(ctx, uploadID) })

	require.NoError(t, s.Append(ctx, uploadID, []byte("hello ")))
	require.NoError(t, s.Append(ctx, uploadID, []byte("world")))

	finalPath, err := s.Finalize(ctx, uploadID)
	require.NoError(t, err)
	require.NotEmpty(t, finalPath)

	got, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(got))

	// Sidecar metadata should exist next to the final file.
	metaBytes, err := os.ReadFile(finalPath + ".ov_upload.meta")
	require.NoError(t, err)
	assert.Contains(t, string(metaBytes), "report.pdf")
	assert.Contains(t, string(metaBytes), `"size": 11`)

	t.Cleanup(func() {
		_ = os.Remove(finalPath)
		_ = os.Remove(finalPath + ".ov_upload.meta")
	})
}

func TestTempUploadStore_CreateAppendFinalize_Shared(t *testing.T) {
	s := newSharedTempStore(t)
	ctx := context.Background()

	dir := s.Config().SharedDir
	f, err := s.Create(ctx, "acct", "data.csv", 1024)
	require.NoError(t, err)
	require.NotNil(t, f)

	metas, err := s.List(ctx, "acct")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	uploadID := metas[0].ID
	t.Cleanup(func() { _ = s.Abort(ctx, uploadID) })

	require.NoError(t, s.Append(ctx, uploadID, []byte("alpha,1\n")))
	require.NoError(t, s.Append(ctx, uploadID, []byte("beta,2\n")))

	finalPath, err := s.Finalize(ctx, uploadID)
	require.NoError(t, err)
	require.NotEmpty(t, finalPath)

	// Final path must live inside the shared directory so other
	// replicas can see the upload.
	assert.True(t, strings.HasPrefix(finalPath, dir),
		"final path %s should be inside shared dir %s", finalPath, dir)

	got, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	assert.Equal(t, "alpha,1\nbeta,2\n", string(got))

	// Original temp file should be gone after the move.
	metas, err = s.List(ctx, "acct")
	require.NoError(t, err)
	assert.Empty(t, metas)
}

func TestTempUploadStore_Abort_RemovesTempFile(t *testing.T) {
	s := newLocalTempStore(t)
	ctx := context.Background()

	f, err := s.Create(ctx, "acct", "abort.bin", 1024)
	require.NoError(t, err)
	tempPath := f.Name()
	require.FileExists(t, tempPath)

	metas, err := s.List(ctx, "acct")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	uploadID := metas[0].ID

	require.NoError(t, s.Abort(ctx, uploadID))

	_, err = os.Stat(tempPath)
	assert.True(t, os.IsNotExist(err), "temp file %s should be removed after abort", tempPath)

	metas, err = s.List(ctx, "acct")
	require.NoError(t, err)
	assert.Empty(t, metas)

	// Abort is idempotent.
	require.NoError(t, s.Abort(ctx, uploadID))
}

func TestTempUploadStore_MaxBytes_Enforced(t *testing.T) {
	s := newLocalTempStore(t)
	ctx := context.Background()

	f, err := s.Create(ctx, "acct", "big.bin", 8)
	require.NoError(t, err)
	require.NotNil(t, f)

	metas, err := s.List(ctx, "acct")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	uploadID := metas[0].ID
	t.Cleanup(func() { _ = s.Abort(ctx, uploadID) })

	// 8 bytes fit, 9th overflows.
	require.NoError(t, s.Append(ctx, uploadID, []byte("12345678")))
	err = s.Append(ctx, uploadID, []byte("9"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrPayloadTooLarge),
		"overflow should be ErrPayloadTooLarge, got %v", err)

	// Finalize succeeds with the truncated buffer.
	finalPath, err := s.Finalize(ctx, uploadID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.Remove(finalPath)
		_ = os.Remove(finalPath + ".ov_upload.meta")
	})
	got, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	assert.Equal(t, "12345678", string(got))
}

func TestTempUploadStore_ConcurrentAppend_RaceFree(t *testing.T) {
	s := newLocalTempStore(t)
	ctx := context.Background()

	// 64 KiB max so many small chunks can land without tripping the
	// limit; we still assert the cumulative byte count matches.
	s.cfg.MaxBytes = 64 * 1024
	f, err := s.Create(ctx, "acct", "race.bin", s.cfg.MaxBytes)
	require.NoError(t, err)
	require.NotNil(t, f)

	metas, err := s.List(ctx, "acct")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	uploadID := metas[0].ID
	t.Cleanup(func() { _ = s.Abort(ctx, uploadID) })

	const goroutines = 16
	const chunksPer = 32
	const chunkSize = 64

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			chunk := make([]byte, chunkSize)
			for j := 0; j < chunksPer; j++ {
				if err := s.Append(ctx, uploadID, chunk); err != nil {
					t.Errorf("Append failed: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	finalPath, err := s.Finalize(ctx, uploadID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.Remove(finalPath)
		_ = os.Remove(finalPath + ".ov_upload.meta")
	})

	info, err := os.Stat(finalPath)
	require.NoError(t, err)
	wantBytes := int64(goroutines * chunksPer * chunkSize)
	assert.Equal(t, wantBytes, info.Size(),
		"final size %d does not match expected %d (lost data race)", info.Size(), wantBytes)
}

func TestTempUploadStore_NewTempUploadStore_RejectsBadMode(t *testing.T) {
	_, err := NewTempUploadStore(config.TempUploadConfig{Mode: "s3"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation), "bad mode should be ErrValidation, got %v", err)
}

func TestTempUploadStore_Finalize_NotFound(t *testing.T) {
	s := newLocalTempStore(t)
	_, err := s.Finalize(context.Background(), "nope")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrNotFound), "missing upload should be ErrNotFound, got %v", err)
}

func TestTempUploadStore_Append_NotFound(t *testing.T) {
	s := newLocalTempStore(t)
	err := s.Append(context.Background(), "nope", []byte("x"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrNotFound), "missing upload should be ErrNotFound, got %v", err)
}

func TestTempUploadStore_List_FiltersByAccount(t *testing.T) {
	s := newLocalTempStore(t)
	ctx := context.Background()

	f1, err := s.Create(ctx, "alice", "a.txt", 1024)
	require.NoError(t, err)
	_ = f1.Close()
	f2, err := s.Create(ctx, "bob", "b.txt", 1024)
	require.NoError(t, err)
	_ = f2.Close()
	t.Cleanup(func() {
		metas, _ := s.List(ctx, "alice")
		for _, m := range metas {
			_ = s.Abort(ctx, m.ID)
		}
		metas, _ = s.List(ctx, "bob")
		for _, m := range metas {
			_ = s.Abort(ctx, m.ID)
		}
	})

	alice, err := s.List(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, alice, 1)
	assert.Equal(t, "alice", alice[0].Account)
	assert.Equal(t, "a.txt", alice[0].Filename)
	assert.Empty(t, alice[0].Path, "Path must not leak across the API")
}
