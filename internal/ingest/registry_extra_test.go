package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistryRegisterLookup verifies Register installs a factory and Lookup
// retrieves it.
func TestRegistryRegisterLookup(t *testing.T) {
	// Use a unique name to avoid colliding with package-level registrations.
	name := "test:fake:register_lookup"
	factory := SourceFactory(func(cfg SourceConfig) (Source, error) {
		return nil, nil
	})
	Register(name, factory)
	defer func() {
		registryMu.Lock()
		delete(registry, name)
		registryMu.Unlock()
	}()

	got, ok := Lookup(name)
	require.True(t, ok)
	require.NotNil(t, got)

	// Unknown name returns ok=false.
	_, ok = Lookup("test:fake:does:not:exist")
	assert.False(t, ok)
}

// TestRegistryRegisteredSorted verifies Registered returns a sorted slice of
// registered names.
func TestRegistryRegisteredSorted(t *testing.T) {
	names := []string{"test:z", "test:a", "test:m"}
	for _, n := range names {
		Register(n, func(cfg SourceConfig) (Source, error) { return nil, nil })
	}
	defer func() {
		registryMu.Lock()
		for _, n := range names {
			delete(registry, n)
		}
		registryMu.Unlock()
	}()

	got := Registered()
	// Find our names in the result and verify they appear in sorted order.
	idxA, idxM, idxZ := -1, -1, -1
	for i, n := range got {
		switch n {
		case "test:a":
			idxA = i
		case "test:m":
			idxM = i
		case "test:z":
			idxZ = i
		}
	}
	require.GreaterOrEqual(t, idxZ, 0)
	assert.Less(t, idxA, idxM)
	assert.Less(t, idxM, idxZ)
}

// TestRegistryMustGet verifies MustGet returns the factory when registered
// and panics when not.
func TestRegistryMustGet(t *testing.T) {
	name := "test:fake:must_get"
	Register(name, func(cfg SourceConfig) (Source, error) { return nil, nil })
	defer func() {
		registryMu.Lock()
		delete(registry, name)
		registryMu.Unlock()
	}()

	require.NotPanics(t, func() { MustGet(name) })
	assert.Panics(t, func() { MustGet("test:fake:not:registered") })
}

// TestRegistryRegisterPanics verifies Register panics on empty name or nil
// factory.
func TestRegistryRegisterPanics(t *testing.T) {
	assert.Panics(t, func() { Register("", func(cfg SourceConfig) (Source, error) { return nil, nil }) })
	assert.Panics(t, func() { Register("test:fake:nil_factory", nil) })
}

// TestBackfillStatsMerge verifies Merge accumulates counters and appends
// errors.
func TestBackfillStatsMerge(t *testing.T) {
	s := &BackfillStats{Sessions: 1, Messages: 10, Committed: 2, Skipped: 3, Errors: []string{"e1"}}
	other := BackfillStats{Sessions: 4, Messages: 50, Committed: 5, Skipped: 6, Errors: []string{"e2", "e3"}}
	s.Merge(other)
	assert.Equal(t, 5, s.Sessions)
	assert.Equal(t, 60, s.Messages)
	assert.Equal(t, 7, s.Committed)
	assert.Equal(t, 9, s.Skipped)
	assert.Equal(t, []string{"e1", "e2", "e3"}, s.Errors)
}

// TestNewOrchestrator verifies NewOrchestrator initializes sources map and
// stores the replayer.
func TestNewOrchestrator(t *testing.T) {
	o := NewOrchestrator(nil, nil)
	require.NotNil(t, o)
	assert.NotNil(t, o.Sources)
	_, ok := o.Sources["nope"]
	assert.False(t, ok)
}
