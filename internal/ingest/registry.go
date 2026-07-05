package ingest

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Source is the contract every harness adapter satisfies. Implementations
// discover conversations in their storage and read them incrementally with
// durable cursors.
//
// Methods must be safe to call from a single goroutine; concurrent calls
// against the same instance are not required. Implementations should
// return domain.ErrUnsupported from Scan when the harness is registered
// but unusable in the current configuration (e.g. legacy file-store).
type Source interface {
	// ID returns the registered harness name, e.g. "claude_code".
	ID() string

	// CursorKind returns the cursor strategy (CursorByteOffset or
	// CursorRowIDTime).
	CursorKind() string

	// IsGroupChat reports whether user turns map to original usernames
	// (true) or to the configured OV user via git identity (false).
	IsGroupChat() bool

	// Scan discovers conversations in the source's storage and invokes fn
	// for each. fn may return an error to stop scanning early; the error
	// is returned to the caller unchanged.
	Scan(ctx context.Context, fn func(SessionRef) error) error

	// Read reads up to limit messages after cursor. Returns messages and
	// the advanced cursor. An empty slice with a possibly-advanced cursor
	// signals EOF for this poll.
	Read(ctx context.Context, ref SessionRef, cursor *Cursor, limit int) ([]NormalizedMessage, *Cursor, error)

	// Close releases any resources held by the source (e.g. a DB handle).
	Close() error
}

// SourceFactory constructs a Source from a SourceConfig. Factories are
// registered with Register and invoked by Lookup at orchestrator build
// time.
type SourceFactory func(cfg SourceConfig) (Source, error)

// SourceConfig is the harness-agnostic configuration handed to a factory.
// Paths overrides the harness's default discovery roots when non-empty.
// User is the fallback OV user for single-user dev harnesses. Settings
// carries any harness-specific knobs (free-form).
type SourceConfig struct {
	Paths    []string       `json:"paths,omitempty"`
	User     string         `json:"user,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

// registry is the package-level Source registry. It mirrors the Python
// SOURCE_REGISTRY dict and @register_source decorator.
var (
	registryMu sync.RWMutex
	registry   = map[string]SourceFactory{}
)

// Register installs a SourceFactory under name. Re-registering an existing
// name overrides the prior factory and is logged via the warning path
// (callers may check Exists to detect collisions).
func Register(name string, factory SourceFactory) {
	if name == "" {
		panic("ingest: register with empty name")
	}
	if factory == nil {
		panic("ingest: register nil factory for " + name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

// Lookup returns the factory registered under name. ok=false when the
// harness is unknown.
func Lookup(name string) (SourceFactory, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}

// Registered returns the harness names currently registered, sorted
// lexicographically. Useful for CLI list-sources.
func Registered() []string {
	registryMu.RLock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	registryMu.RUnlock()
	sort.Strings(names)
	return names
}

// MustGet panics when name is not registered. Intended for use in main()
// initialization so missing adapters fail loudly at startup.
func MustGet(name string) SourceFactory {
	f, ok := Lookup(name)
	if !ok {
		panic(fmt.Sprintf("ingest: source %q not registered", name))
	}
	return f
}
