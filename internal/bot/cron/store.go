// Package cron - persistent job store for CLI management of scheduled
// tasks. The runtime Scheduler wraps gocron for in-process scheduling;
// the Store interface defined here captures the CLI's needs (list, add,
// remove, enable/disable, trigger) so `ov cron` can manage jobs without
// a running bot process.
//
// Two implementations are provided:
//   - MemoryStore: in-memory map, used by tests.
//   - FileStore: JSON file at <data_dir>/cron/jobs.json, mirrors the
//     python vikingbot CronService persistence model.
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Job is the persisted representation of a scheduled task. It mirrors
// the fields the CLI surfaces: an identifier, a cron schedule, a task
// type, an optional JSON payload, an enabled flag, and the next-run
// hint computed by the runtime scheduler.
type Job struct {
	// ID is the unique job identifier (UUID). Assigned by Add.
	ID string `json:"id"`
	// Schedule is the cron expression (5-field or 6-field with seconds).
	Schedule string `json:"schedule"`
	// Type identifies the task handler (e.g. "reminder", "cleanup").
	Type string `json:"type"`
	// Payload is the opaque JSON payload passed to the handler.
	Payload []byte `json:"payload,omitempty"`
	// Enabled gates whether the runtime scheduler runs the job.
	Enabled bool `json:"enabled"`
	// NextRun is the next scheduled run time. Zero when not scheduled.
	NextRun time.Time `json:"next_run,omitempty"`
	// CreatedAt is when the job was added.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the last modification time.
	UpdatedAt time.Time `json:"updated_at"`
}

// JobStore is the interface the CLI uses to manage cron jobs. The
// runtime Scheduler can satisfy it (delegating to gocron) or a separate
// persistence layer can satisfy it (FileStore).
type JobStore interface {
	// List returns all jobs. Disabled jobs are included.
	List(ctx context.Context) ([]Job, error)
	// Add creates a new job and returns its ID.
	Add(ctx context.Context, schedule, typ string, payload []byte) (string, error)
	// Remove deletes a job by ID. Returns an error matching ErrJobNotFound
	// when the ID does not exist.
	Remove(ctx context.Context, id string) error
	// SetEnabled toggles a job's enabled state.
	SetEnabled(ctx context.Context, id string, enabled bool) error
	// Run triggers a one-shot execution of the job. It does not affect
	// the schedule. Returns an error matching ErrJobNotFound when the ID
	// does not exist, or ErrJobDisabled when the job is disabled.
	Run(ctx context.Context, id string) error
}

// ErrJobNotFound is returned when a job ID does not exist in the store.
var ErrJobNotFound = fmt.Errorf("cron: job not found")

// ErrJobDisabled is returned by Run when the target job is disabled.
var ErrJobDisabled = fmt.Errorf("cron: job disabled")

// MemoryStore is an in-memory JobStore. It is safe for concurrent use
// and is primarily used by tests.
type MemoryStore struct {
	mu   sync.Mutex
	jobs map[string]Job
	// RunHook is invoked by Run when non-nil, after the job is resolved
	// and before nil is returned. Useful for asserting in tests that
	// Run was called with the expected ID.
	RunHook func(ctx context.Context, id string) error
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: make(map[string]Job)}
}

// List returns all jobs in insertion order (map iteration is randomized
// in Go; callers that need stable order should sort the result).
func (m *MemoryStore) List(_ context.Context) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	return out, nil
}

// Add creates a new job with a generated UUID and returns its ID.
func (m *MemoryStore) Add(_ context.Context, schedule, typ string, payload []byte) (string, error) {
	now := time.Now().UTC()
	id := uuid.NewString()
	j := Job{
		ID:        id,
		Schedule:  schedule,
		Type:      typ,
		Payload:   payload,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.mu.Lock()
	m.jobs[id] = j
	m.mu.Unlock()
	return id, nil
}

// Remove deletes a job by ID.
func (m *MemoryStore) Remove(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[id]; !ok {
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	delete(m.jobs, id)
	return nil
}

// SetEnabled toggles a job's enabled state.
func (m *MemoryStore) SetEnabled(_ context.Context, id string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	j.Enabled = enabled
	j.UpdatedAt = time.Now().UTC()
	m.jobs[id] = j
	return nil
}

// Run triggers a one-shot execution. For the in-memory store this is
// a no-op apart from invoking RunHook; the runtime scheduler performs
// the actual execution.
func (m *MemoryStore) Run(ctx context.Context, id string) error {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	if !j.Enabled {
		return fmt.Errorf("%w: %s", ErrJobDisabled, id)
	}
	if m.RunHook != nil {
		return m.RunHook(ctx, id)
	}
	return nil
}

// FileStore is a JSON file-backed JobStore. It persists jobs to a single
// file at construction time. The on-disk format is a JSON object mapping
// job ID -> Job. Concurrent processes are not safe; the CLI assumes
// exclusive access while it runs.
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore returns a FileStore at the given path. The file (and
// parent directories) are created on first Add.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// DefaultStorePath returns the default path for the cron job store:
// $OV_DATA_DIR/cron/jobs.json, or ~/.openviking/cron/jobs.json when
// OV_DATA_DIR is unset.
func DefaultStorePath() (string, error) {
	if v := os.Getenv("OV_DATA_DIR"); v != "" {
		return filepath.Join(v, "cron", "jobs.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cron: home dir: %w", err)
	}
	return filepath.Join(home, ".openviking", "cron", "jobs.json"), nil
}

// load reads the file and returns the job map. Missing file is not an
// error: an empty map is returned.
func (s *FileStore) load() (map[string]Job, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]Job), nil
		}
		return nil, fmt.Errorf("cron: read store: %w", err)
	}
	if len(data) == 0 {
		return make(map[string]Job), nil
	}
	var jobs map[string]Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("cron: parse store: %w", err)
	}
	return jobs, nil
}

// save writes the job map back to disk, creating parent directories.
func (s *FileStore) save(jobs map[string]Job) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("cron: mkdir %s: %w", filepath.Dir(s.path), err)
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return fmt.Errorf("cron: marshal store: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("cron: write store: %w", err)
	}
	return nil
}

// List returns all jobs from the file.
func (s *FileStore) List(_ context.Context) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j)
	}
	return out, nil
}

// Add creates a new job and persists it.
func (s *FileStore) Add(_ context.Context, schedule, typ string, payload []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.load()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	id := uuid.NewString()
	jobs[id] = Job{
		ID:        id,
		Schedule:  schedule,
		Type:      typ,
		Payload:   payload,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.save(jobs); err != nil {
		return "", err
	}
	return id, nil
}

// Remove deletes a job by ID and persists the change.
func (s *FileStore) Remove(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := jobs[id]; !ok {
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	delete(jobs, id)
	return s.save(jobs)
}

// SetEnabled toggles a job's enabled state and persists the change.
func (s *FileStore) SetEnabled(_ context.Context, id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.load()
	if err != nil {
		return err
	}
	j, ok := jobs[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	j.Enabled = enabled
	j.UpdatedAt = time.Now().UTC()
	jobs[id] = j
	return s.save(jobs)
}

// Run triggers a one-shot execution. For the file-backed store this is
// a no-op apart from validating the job exists and is enabled; the
// runtime scheduler performs the actual execution.
func (s *FileStore) Run(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.load()
	if err != nil {
		return err
	}
	j, ok := jobs[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	if !j.Enabled {
		return fmt.Errorf("%w: %s", ErrJobDisabled, id)
	}
	return nil
}
