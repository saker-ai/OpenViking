package ragfs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// OpType labels a sync log entry's operation kind.
type OpType string

const (
	OpWrite   OpType = "WRITE"
	OpSync    OpType = "SYNC"
	OpPending OpType = "PENDING"
	OpDelete  OpType = "DELETE"
)

// SyncLogEntry is one row of `.sync_log.json`.
//
// The schema matches the Rust ragfs sync log so the file is interoperable
// across implementations.
type SyncLogEntry struct {
	Op         OpType    `json:"op"`
	Path       string    `json:"path"`
	Backend    string    `json:"backend"`
	Timestamp  time.Time `json:"ts"`
	RetryCount int       `json:"retry_count,omitempty"`
	Hash       string    `json:"hash,omitempty"`
	Status     string    `json:"status,omitempty"`
}

// SyncLogStore appends and reads `.sync_log.json` sidecars. It is the
// consistency ledger used by MultiWriteFS to track which backups have
// replicated a write and which are still pending.
//
// The store keeps an in-memory copy for fast reads and flushes to disk on
// every append. It is safe for concurrent use.
type SyncLogStore struct {
	path    string
	mu      sync.Mutex
	entries []SyncLogEntry
}

// NewSyncLogStore opens (or creates) a sync log at the given filesystem
// path. The path must be absolute and must point at the `.sync_log.json`
// sidecar location, NOT a ragfs resource path.
func NewSyncLogStore(path string) *SyncLogStore {
	return &SyncLogStore{path: path}
}

// Append records a new entry and flushes to disk atomically.
func (s *SyncLogStore) Append(ctx context.Context, e SyncLogEntry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return s.flushLocked()
}

// AppendBatch records multiple entries in one flush.
func (s *SyncLogStore) AppendBatch(ctx context.Context, entries []SyncLogEntry) error {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range entries {
		if entries[i].Timestamp.IsZero() {
			entries[i].Timestamp = now
		}
		s.entries = append(s.entries, entries[i])
	}
	return s.flushLocked()
}

// Entries returns a copy of the current log.
func (s *SyncLogStore) Entries() []SyncLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SyncLogEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Pending returns entries that have not yet been synced to all backups
// (i.e. OpPending with RetryCount below maxRetries).
func (s *SyncLogStore) Pending(maxRetries int) []SyncLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SyncLogEntry
	for _, e := range s.entries {
		if e.Op == OpPending && (maxRetries == 0 || e.RetryCount < maxRetries) {
			out = append(out, e)
		}
	}
	return out
}

// Load re-reads the on-disk log into memory, replacing the current state.
func (s *SyncLogStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.entries = nil
			return nil
		}
		return wrapRAGFS(err)
	}
	if len(data) == 0 {
		s.entries = nil
		return nil
	}
	var entries []SyncLogEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return wrapRAGFS(err)
	}
	s.entries = entries
	return nil
}

// flushLocked serialises the in-memory log to disk. Caller must hold s.mu.
func (s *SyncLogStore) flushLocked() error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.entries); err != nil {
		return wrapRAGFS(err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return wrapRAGFS(err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return wrapRAGFS(err)
	}
	return nil
}

// LoadSyncLog reads a `.sync_log.json` sidecar from a FileSystem backend
// without an in-memory store. Returns an empty slice (no error) when the
// sidecar is missing.
func LoadSyncLog(ctx context.Context, fs FileSystem, resourcePath string) ([]SyncLogEntry, error) {
	sidecar := HiddenSidecar(resourcePath, ".sync_log.json")
	var buf bytes.Buffer
	if err := fs.Read(ctx, sidecar, &buf); err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []SyncLogEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		return nil, wrapRAGFS(err)
	}
	return entries, nil
}

// WriteSyncLog writes a `.sync_log.json` sidecar to a FileSystem backend.
func WriteSyncLog(ctx context.Context, fs FileSystem, resourcePath string, entries []SyncLogEntry) error {
	sidecar := HiddenSidecar(resourcePath, ".sync_log.json")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entries); err != nil {
		return wrapRAGFS(err)
	}
	return fs.Write(ctx, sidecar, bytes.NewReader(buf.Bytes()), 0o644)
}
