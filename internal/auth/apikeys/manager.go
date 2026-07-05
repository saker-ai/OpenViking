// Package apikeys provides an API key manager that stores keys as argon2id
// hashes with an in-memory index by key_prefix for O(1) lookup. The
// plaintext key is returned only at generation time; persisted records
// contain only the hash, key_prefix (first 8 chars), and a sha256
// fingerprint for display.
//
// Storage is a single JSON file at a configurable path, written
// atomically via tmpfile+rename. The Manager is concurrent-safe for
// Generate/Verify/List/Revoke/Persist; the argon2id verification is
// performed under the read lock so revocation is never observed mid-flight.
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/saker-ai/ctxhub/internal/auth"
)

// KeyLen is the length of the generated plaintext key in bytes. The
// encoded hex form is 2*KeyLen chars.
const KeyLen = 32

// PrefixLen is the length of the key_prefix index key (first N chars of
// the hex-encoded plaintext). Matches the Python reference default.
const PrefixLen = 8

// APIKeyRecord is the persisted shape of an API key. The Hash field
// holds an argon2id encoded hash; the plaintext key is never persisted.
// AccountID and Scope are optional metadata set by callers (e.g. the
// admin router) after Generate returns.
type APIKeyRecord struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	AccountID   string    `json:"account_id,omitempty"`
	Scope       string    `json:"scope,omitempty"`
	KeyPrefix   string    `json:"key_prefix"`
	Fingerprint string    `json:"fingerprint"`
	Hash        string    `json:"hash"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
	RevokedAt   time.Time `json:"revoked_at,omitempty"`
}

// Revoked reports whether the key has been soft-revoked.
func (r *APIKeyRecord) Revoked() bool {
	return r != nil && !r.RevokedAt.IsZero()
}

// Manager stores API keys in memory with an index by key_prefix for O(1)
// lookup. Persist writes the records to a JSON file at the configured
// path via tmpfile+rename for atomicity.
type Manager struct {
	mu       sync.RWMutex
	path     string
	records  map[string]*APIKeyRecord   // by ID
	byPrefix map[string][]*APIKeyRecord // by key_prefix
}

// NewManager constructs a Manager backed by the JSON file at path. The
// file is loaded if it exists; missing file is not an error. The caller
// must call Persist to flush changes.
func NewManager(path string) (*Manager, error) {
	m := &Manager{
		path:     path,
		records:  make(map[string]*APIKeyRecord),
		byPrefix: make(map[string][]*APIKeyRecord),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// Generate creates a new API key with a random 32-byte plaintext. The
// plaintext is returned once; only the argon2id hash is persisted in the
// returned record. The caller should set AccountID/Scope on the
// returned record (under the Manager's mutex via SetAccountScope) and
// call Persist to flush the new record to disk.
func (m *Manager) Generate(name string) (key string, record *APIKeyRecord, err error) {
	raw := make([]byte, KeyLen)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("apikeys: read key: %w", err)
	}
	key = hex.EncodeToString(raw)
	hash, err := auth.Hash(key, auth.DefaultParams)
	if err != nil {
		return "", nil, fmt.Errorf("apikeys: hash key: %w", err)
	}
	now := time.Now().UTC()
	record = &APIKeyRecord{
		ID:          uuid.NewString(),
		Name:        name,
		KeyPrefix:   key[:PrefixLen],
		Fingerprint: fingerprint(key),
		Hash:        hash,
		CreatedAt:   now,
	}
	m.mu.Lock()
	m.records[record.ID] = record
	m.byPrefix[record.KeyPrefix] = append(m.byPrefix[record.KeyPrefix], record)
	m.mu.Unlock()
	return key, record, nil
}

// Verify looks up the record by the first PrefixLen chars of the
// plaintext (key_prefix) and constant-time verifies the argon2id hash.
// Returns ErrNotFound when no record matches the prefix, ErrRevoked
// when the matching record is soft-revoked.
func (m *Manager) Verify(plaintext string) (*APIKeyRecord, error) {
	if len(plaintext) < PrefixLen {
		return nil, ErrNotFound
	}
	prefix := plaintext[:PrefixLen]
	m.mu.RLock()
	defer m.mu.RUnlock()
	candidates := m.byPrefix[prefix]
	for _, r := range candidates {
		ok, err := auth.Verify(plaintext, r.Hash)
		if err != nil || !ok {
			continue
		}
		if r.Revoked() {
			return r, ErrRevoked
		}
		return r, nil
	}
	return nil, ErrNotFound
}

// List returns all records (including revoked ones) sorted by CreatedAt.
func (m *Manager) List() []*APIKeyRecord {
	m.mu.RLock()
	out := make([]*APIKeyRecord, 0, len(m.records))
	for _, r := range m.records {
		out = append(out, r)
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Get returns the record with the given ID, or ErrNotFound if no such
// record exists.
func (m *Manager) Get(id string) (*APIKeyRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.records[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r, nil
}

// SetAccountScope updates the AccountID and Scope metadata on a record.
// Callers should use this rather than mutating the record pointer
// directly so the Manager's mutex protects the write.
func (m *Manager) SetAccountScope(id, accountID, scope string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok {
		return ErrNotFound
	}
	r.AccountID = accountID
	r.Scope = scope
	return nil
}

// Revoke marks the record with the given ID as soft-revoked by setting
// RevokedAt. Returns ErrNotFound when the ID is unknown. Re-revoking a
// already-revoked record is a no-op.
func (m *Manager) Revoke(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok {
		return ErrNotFound
	}
	if r.RevokedAt.IsZero() {
		r.RevokedAt = time.Now().UTC()
	}
	return nil
}

// Persist atomically writes the records to the JSON file at m.path via
// tmpfile+rename. The directory is created if missing. No-op when path
// is empty (in-memory mode).
func (m *Manager) Persist() error {
	m.mu.RLock()
	out := make([]*APIKeyRecord, 0, len(m.records))
	for _, r := range m.records {
		out = append(out, r)
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	payload := struct {
		APIKeys []*APIKeyRecord `json:"api_keys"`
	}{APIKeys: out}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("apikeys: marshal: %w", err)
	}
	if m.path == "" {
		return nil
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("apikeys: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".apikeys-*.tmp")
	if err != nil {
		return fmt.Errorf("apikeys: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("apikeys: write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("apikeys: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return fmt.Errorf("apikeys: rename: %w", err)
	}
	return nil
}

// load reads the JSON file at m.path into memory. Missing file is not an
// error; the manager starts empty.
func (m *Manager) load() error {
	if m.path == "" {
		return nil
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("apikeys: read %s: %w", m.path, err)
	}
	var payload struct {
		APIKeys []*APIKeyRecord `json:"api_keys"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("apikeys: parse %s: %w", m.path, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range payload.APIKeys {
		if r == nil || r.ID == "" {
			continue
		}
		// Deduplicate in case the file has stale entries.
		if _, exists := m.records[r.ID]; exists {
			continue
		}
		m.records[r.ID] = r
		if r.KeyPrefix != "" {
			m.byPrefix[r.KeyPrefix] = append(m.byPrefix[r.KeyPrefix], r)
		}
	}
	return nil
}

// fingerprint returns the sha256 of the plaintext key, truncated to 8
// hex chars for display. Not used for lookup — only for human-readable
// identification in admin UIs.
func fingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:8]
}

// Sentinel errors. Use errors.Is to match.
var (
	// ErrNotFound is returned when no record matches a lookup.
	ErrNotFound = errors.New("apikeys: not found")
	// ErrRevoked is returned when the matching record is soft-revoked.
	ErrRevoked = errors.New("apikeys: revoked")
)
