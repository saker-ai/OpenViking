// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

// Package server temp_upload_store.go: temp file store for HTTP server
// uploads. Mirrors the Python TempUploadStore in
// openviking/server/temp_upload_store.py.
//
// Two modes:
//
//   - local: files live in os.TempDir() under the "openviking-upload-*"
//     pattern. Single-replica deployments.
//   - shared: files live in a configured shared directory (default
//     "./data/uploads/"). Multi-replica deployments where every server
//     replica can see the same upload.
//
// Lock integration: cross-process exclusive access during Finalize (shared
// mode) is guarded by an O_EXCL lockfile at <shared>/<id>.lock. If the
// lockfile already exists, another replica is consuming the upload and
// Finalize returns ErrConflict.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// UploadMeta is the metadata snapshot for a pending upload returned by
// TempUploadStore.List. Path is the local temp file path; it is zeroed
// before returning to callers so the host filesystem layout is not leaked
// across the public API.
type UploadMeta struct {
	ID            string    `json:"id"`
	Account       string    `json:"account"`
	Filename      string    `json:"filename"`
	BytesReceived int64     `json:"bytes_received"`
	MaxBytes      int64     `json:"max_bytes"`
	CreatedAt     time.Time `json:"created_at"`
	Mode          string    `json:"mode"`
	Path          string    `json:"-"`
}

// TempUploadStore buffers HTTP server uploads on disk before ingestion.
// Construction is via NewTempUploadStore; the zero value is not usable.
type TempUploadStore struct {
	cfg config.TempUploadConfig

	// mu guards the uploads map. Per-upload serialization is handled by
	// uploadEntry.mu so concurrent Appends on different uploads do not
	// contend with each other.
	mu      sync.Mutex
	uploads map[string]*uploadEntry
}

// uploadEntry is the in-memory record for a single pending upload.
type uploadEntry struct {
	meta UploadMeta
	file *os.File
	mu   sync.Mutex
}

// NewTempUploadStore constructs a TempUploadStore from cfg. Returns
// domain.ErrValidation when Mode is not "local" or "shared". When Mode
// is "shared" the SharedDir is created with mode 0755 if it does not
// exist. Empty MaxBytes is filled from the configured default.
func NewTempUploadStore(cfg config.TempUploadConfig) (*TempUploadStore, error) {
	switch cfg.Mode {
	case "local", "shared":
	case "":
		cfg.Mode = "local"
	default:
		return nil, fmt.Errorf("%w: temp_upload mode must be 'local' or 'shared'", domain.ErrValidation)
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 1 << 30 // 1 GiB default, mirrors defaults.go.
	}
	if cfg.Mode == "shared" {
		if cfg.SharedDir == "" {
			cfg.SharedDir = "./data/uploads/"
		}
		if err := os.MkdirAll(cfg.SharedDir, 0o755); err != nil {
			return nil, fmt.Errorf("temp_upload_store: mkdir shared dir: %w", err)
		}
	}
	return &TempUploadStore{
		cfg:     cfg,
		uploads: make(map[string]*uploadEntry),
	}, nil
}

// Config returns the resolved configuration. Useful for tests.
func (s *TempUploadStore) Config() config.TempUploadConfig { return s.cfg }

// Create opens a new temp upload file and registers it under a fresh
// upload ID. The returned *os.File is owned by the store; callers may
// write to it directly or use Append. maxBytes <= 0 falls back to the
// store default; uploads exceeding maxBytes are refused at Append time
// with domain.ErrPayloadTooLarge.
func (s *TempUploadStore) Create(ctx context.Context, account, filename string, maxBytes int64) (*os.File, error) {
	if account == "" {
		return nil, fmt.Errorf("%w: account is required", domain.ErrValidation)
	}
	if maxBytes <= 0 {
		maxBytes = s.cfg.MaxBytes
	}
	uploadID, err := newUploadID()
	if err != nil {
		return nil, fmt.Errorf("temp_upload_store: generate id: %w", err)
	}
	pattern := "openviking-upload-*" + tempSuffix(filename)
	f, err := os.CreateTemp(s.tempDir(), pattern)
	if err != nil {
		return nil, fmt.Errorf("temp_upload_store: create temp file: %w", err)
	}
	entry := &uploadEntry{
		meta: UploadMeta{
			ID:        uploadID,
			Account:   account,
			Filename:  filename,
			MaxBytes:  maxBytes,
			CreatedAt: time.Now().UTC(),
			Mode:      s.cfg.Mode,
			Path:      f.Name(),
		},
		file: f,
	}
	s.mu.Lock()
	s.uploads[uploadID] = entry
	s.mu.Unlock()
	return f, nil
}

// Append writes chunk to the upload identified by uploadID. The chunk
// is written atomically with respect to other concurrent Appends on the
// same upload (per-upload mutex). Returns domain.ErrPayloadTooLarge
// when the cumulative byte count would exceed MaxBytes.
func (s *TempUploadStore) Append(ctx context.Context, uploadID string, chunk []byte) error {
	s.mu.Lock()
	entry, ok := s.uploads[uploadID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: upload not found", domain.ErrNotFound)
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.meta.BytesReceived+int64(len(chunk)) > entry.meta.MaxBytes {
		return fmt.Errorf("%w: upload exceeds size limit (%d bytes)", domain.ErrPayloadTooLarge, entry.meta.MaxBytes)
	}
	n, err := entry.file.Write(chunk)
	if err != nil {
		return fmt.Errorf("temp_upload_store: append: %w", err)
	}
	entry.meta.BytesReceived += int64(n)
	return nil
}

// Finalize closes the upload and returns the final on-disk path. In
// shared mode the temp file is moved into SharedDir and the moved path
// is returned; in local mode the original temp path is returned. A
// sidecar metadata file "<path>.ov_upload.meta" is written so consumers
// can recover the original filename and size. Cross-process concurrency
// is guarded by an O_EXCL lockfile; if the lock is held by another
// process, Finalize returns domain.ErrConflict.
func (s *TempUploadStore) Finalize(ctx context.Context, uploadID string) (string, error) {
	s.mu.Lock()
	entry, ok := s.uploads[uploadID]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("%w: upload not found", domain.ErrNotFound)
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.file != nil {
		if err := entry.file.Close(); err != nil {
			return "", fmt.Errorf("temp_upload_store: close: %w", err)
		}
		entry.file = nil
	}
	finalPath := entry.meta.Path
	if s.cfg.Mode == "shared" {
		lockPath := filepath.Join(s.cfg.SharedDir, uploadID+".lock")
		lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", fmt.Errorf("%w: temporary upload is being consumed", domain.ErrConflict)
		}
		defer func() {
			_ = lock.Close()
			_ = os.Remove(lockPath)
		}()
		finalPath = filepath.Join(s.cfg.SharedDir, "openviking-upload-"+uploadID+tempSuffix(entry.meta.Filename))
		if err := moveFile(entry.meta.Path, finalPath); err != nil {
			return "", fmt.Errorf("temp_upload_store: move to shared: %w", err)
		}
		entry.meta.Path = finalPath
	}
	metaPath := finalPath + ".ov_upload.meta"
	if err := writeJSONFile(metaPath, map[string]any{
		"upload_id":         entry.meta.ID,
		"account":           entry.meta.Account,
		"original_filename": entry.meta.Filename,
		"size":              entry.meta.BytesReceived,
		"max_bytes":         entry.meta.MaxBytes,
		"upload_time":       entry.meta.CreatedAt.Unix(),
		"mode":              entry.meta.Mode,
	}); err != nil {
		return "", fmt.Errorf("temp_upload_store: write meta: %w", err)
	}
	s.mu.Lock()
	delete(s.uploads, uploadID)
	s.mu.Unlock()
	return finalPath, nil
}

// Abort deletes the temp file and removes the upload from the registry.
// It is idempotent: calling Abort on an unknown or already-aborted ID
// returns nil.
func (s *TempUploadStore) Abort(ctx context.Context, uploadID string) error {
	s.mu.Lock()
	entry, ok := s.uploads[uploadID]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.uploads, uploadID)
	s.mu.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.file != nil {
		_ = entry.file.Close()
		entry.file = nil
	}
	if entry.meta.Path != "" {
		_ = os.Remove(entry.meta.Path)
	}
	return nil
}

// List returns metadata for all pending uploads belonging to account.
// The Path field is zeroed so the host filesystem layout is not leaked.
func (s *TempUploadStore) List(ctx context.Context, account string) ([]UploadMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UploadMeta, 0)
	for _, e := range s.uploads {
		if e.meta.Account != account {
			continue
		}
		e.mu.Lock()
		snapshot := e.meta
		e.mu.Unlock()
		snapshot.Path = ""
		out = append(out, snapshot)
	}
	return out, nil
}

// tempDir returns the directory used for new temp files: os.TempDir()
// for local mode (mirrors the Python "openviking-upload-*" pattern in
// tempfile.get_upload_temp_dir), or SharedDir for shared mode.
func (s *TempUploadStore) tempDir() string {
	if s.cfg.Mode == "shared" {
		return s.cfg.SharedDir
	}
	return os.TempDir()
}

// tempSuffix returns the file extension used for the temp file pattern.
// Empty filenames default to ".tmp" so CreateTemp always has a suffix.
func tempSuffix(filename string) string {
	if ext := filepath.Ext(filename); ext != "" {
		return ext
	}
	return ".tmp"
}

// newUploadID returns a 32-char hex ID minted from 16 random bytes.
// crypto/rand is used so IDs are unpredictable across replicas.
func newUploadID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeJSONFile writes v as pretty-printed JSON to path with mode 0644.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// moveFile renames src to dst, falling back to a copy+remove when src
// and dst live on different filesystems (EXDEV). The fallback matches
// the Python _save_shared behaviour which copies bytes into viking_fs.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}
