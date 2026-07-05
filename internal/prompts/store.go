package prompts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
)

// Store is a hot-reloadable collection of prompt templates. It loads
// templates from the embedded archive by default, and from a
// configurable on-disk directory when set. Reload re-scans the disk
// directory (or the embedded archive when no dir is configured) and
// atomically swaps the template map.
//
// Store is safe for concurrent use: Load, Get, Render, and Reload may
// be called from any goroutine. Reads fetch the active snapshot via
// atomic.Pointer; writes build a new snapshot and atomically swap it
// in.
type Store struct {
	dir string // on-disk override directory; empty means embed-only

	// reloads counts how many times Reload has rebuilt the snapshot.
	// Exposed for tests and diagnostics via ReloadCount().
	reloads atomic.Int64

	mu       sync.Mutex // protects snapshots during multi-step loads
	snapshot atomic.Pointer[snapshot]
}

// snapshot is an immutable view of the loaded templates. Replaced
// atomically on Reload.
type snapshot struct {
	templates  map[string]*Template
	source     string // "embed" or absolute disk path
	loadedFrom string // human-readable source label for diagnostics
}

// NewStore creates a Store. If dir is non-empty, templates are loaded
// from that on-disk directory (and watched for hot reload when
// Watch is called). When dir is empty, the embedded archive is used.
func NewStore(dir string) *Store {
	s := &Store{dir: dir}
	return s
}

// Dir returns the configured on-disk override directory, or "" when
// the store is embed-only.
func (s *Store) Dir() string { return s.dir }

// Load populates the store from the embedded archive or the on-disk
// directory. Calling Load replaces any previously loaded snapshot.
// Returns ErrNoTemplates when no YAML files are found.
func (s *Store) Load() error {
	loaded, source, err := s.loadFromConfigured()
	if err != nil {
		return err
	}
	snap := &snapshot{
		templates:  loaded,
		source:     source,
		loadedFrom: source,
	}
	s.snapshot.Store(snap)
	return nil
}

// loadFromConfigured returns the template map and a human-readable
// source label, using the disk directory when set or the embedded
// archive otherwise.
func (s *Store) loadFromConfigured() (map[string]*Template, string, error) {
	if s.dir != "" {
		abs, err := filepath.Abs(s.dir)
		if err != nil {
			return nil, "", fmt.Errorf("prompts: resolve dir %s: %w", s.dir, err)
		}
		if _, statErr := os.Stat(abs); statErr != nil {
			return nil, "", fmt.Errorf("prompts: dir %s: %w", abs, statErr)
		}
		loaded, err := loadDisk(abs)
		if err != nil {
			return nil, "", err
		}
		return loaded, abs, nil
	}
	loaded, err := loadEmbedded()
	if err != nil {
		return nil, "", err
	}
	return loaded, "embed", nil
}

// Reload re-scans the configured source and atomically swaps the
// active snapshot. Concurrent Get/Render calls continue to use the
// previous snapshot until the swap completes. Returns the new
// template count.
func (s *Store) Reload() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded, source, err := s.loadFromConfigured()
	if err != nil {
		return 0, err
	}
	snap := &snapshot{
		templates:  loaded,
		source:     source,
		loadedFrom: source,
	}
	s.snapshot.Store(snap)
	s.reloads.Add(1)
	return len(loaded), nil
}

// ReloadCount returns the number of times Reload has rebuilt the
// snapshot. Useful for tests that assert a hot reload occurred.
func (s *Store) ReloadCount() int64 {
	return s.reloads.Load()
}

// Get returns the template with the given prompt ID (e.g.
// "vision.image_understanding"). Returns ErrTemplateNotFound when the
// ID is not in the active snapshot. The returned Template is shared
// and must not be mutated.
func (s *Store) Get(name string) (*Template, error) {
	snap := s.snapshot.Load()
	if snap == nil {
		return nil, ErrStoreNotLoaded
	}
	t, ok := snap.templates[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTemplateNotFound, name)
	}
	return t, nil
}

// Render loads (when needed), looks up, and renders the named
// template's primary "template" field with the given variables.
// Equivalent to Get + Template.Render.
func (s *Store) Render(name string, vars map[string]any) (string, error) {
	t, err := s.Get(name)
	if err != nil {
		return "", err
	}
	return t.Render(vars)
}

// List returns the sorted prompt IDs in the active snapshot. Returns
// ErrStoreNotLoaded when Load has not been called.
func (s *Store) List() ([]string, error) {
	snap := s.snapshot.Load()
	if snap == nil {
		return nil, ErrStoreNotLoaded
	}
	ids := make([]string, 0, len(snap.templates))
	for id := range snap.templates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// Source returns a human-readable label for the active snapshot's
// source ("embed" or the absolute disk path). Empty when not loaded.
func (s *Store) Source() string {
	snap := s.snapshot.Load()
	if snap == nil {
		return ""
	}
	return snap.loadedFrom
}

// Errors surfaced by Store.
var (
	ErrStoreNotLoaded   = errors.New("prompts: store not loaded")
	ErrTemplateNotFound = errors.New("prompts: template not found")
)
