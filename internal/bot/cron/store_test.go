package cron

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStore_AddListRemove(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	id, err := s.Add(ctx, "0 9 * * *", "reminder", []byte(`{"msg":"hi"}`))
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	jobs, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "0 9 * * *", jobs[0].Schedule)
	assert.Equal(t, "reminder", jobs[0].Type)
	assert.True(t, jobs[0].Enabled)
	assert.Equal(t, `{"msg":"hi"}`, string(jobs[0].Payload))

	require.NoError(t, s.Remove(ctx, id))
	jobs, err = s.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, jobs)
}

func TestMemoryStore_RemoveNotFound(t *testing.T) {
	s := NewMemoryStore()
	err := s.Remove(context.Background(), "nope")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrJobNotFound))
}

func TestMemoryStore_SetEnabled(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	id, _ := s.Add(ctx, "* * * * *", "tick", nil)

	require.NoError(t, s.SetEnabled(ctx, id, false))
	jobs, _ := s.List(ctx)
	assert.False(t, jobs[0].Enabled)

	require.NoError(t, s.SetEnabled(ctx, id, true))
	jobs, _ = s.List(ctx)
	assert.True(t, jobs[0].Enabled)
}

func TestMemoryStore_SetEnabledNotFound(t *testing.T) {
	s := NewMemoryStore()
	err := s.SetEnabled(context.Background(), "nope", true)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrJobNotFound))
}

func TestMemoryStore_Run(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	id, _ := s.Add(ctx, "* * * * *", "tick", nil)

	var called string
	s.RunHook = func(_ context.Context, got string) error {
		called = got
		return nil
	}
	require.NoError(t, s.Run(ctx, id))
	assert.Equal(t, id, called)
}

func TestMemoryStore_RunDisabled(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	id, _ := s.Add(ctx, "* * * * *", "tick", nil)
	require.NoError(t, s.SetEnabled(ctx, id, false))

	err := s.Run(ctx, id)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrJobDisabled))
}

func TestMemoryStore_RunNotFound(t *testing.T) {
	s := NewMemoryStore()
	err := s.Run(context.Background(), "nope")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrJobNotFound))
}

func TestFileStore_AddListRemove(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	s := NewFileStore(path)

	id, err := s.Add(ctx, "0 9 * * *", "reminder", []byte(`{"msg":"hi"}`))
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Re-open the store to verify persistence.
	s2 := NewFileStore(path)
	jobs, err := s2.List(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, id, jobs[0].ID)
	assert.Equal(t, "reminder", jobs[0].Type)
	assert.True(t, jobs[0].Enabled)

	require.NoError(t, s2.Remove(ctx, id))
	jobs, err = s2.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, jobs)
}

func TestFileStore_SetEnabled(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.json")
	s := NewFileStore(path)
	id, _ := s.Add(ctx, "* * * * *", "tick", nil)

	require.NoError(t, s.SetEnabled(ctx, id, false))
	s2 := NewFileStore(path)
	jobs, _ := s2.List(ctx)
	require.Len(t, jobs, 1)
	assert.False(t, jobs[0].Enabled)
}

func TestFileStore_RunDisabled(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	id, _ := s.Add(ctx, "* * * * *", "tick", nil)
	require.NoError(t, s.SetEnabled(ctx, id, false))

	err := s.Run(ctx, id)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrJobDisabled))
}

func TestFileStore_RemoveNotFound(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	err := s.Remove(context.Background(), "nope")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrJobNotFound))
}

func TestDefaultStorePath_Env(t *testing.T) {
	t.Setenv("OV_DATA_DIR", "/tmp/ov-test")
	p, err := DefaultStorePath()
	require.NoError(t, err)
	assert.Equal(t, "/tmp/ov-test/cron/jobs.json", p)
}
