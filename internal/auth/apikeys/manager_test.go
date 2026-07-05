package apikeys

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(filepath.Join(t.TempDir(), "apikeys.json"))
	require.NoError(t, err)
	return m
}

func TestManager_GenerateVerifyListRevoke(t *testing.T) {
	m := newTestManager(t)

	key, rec, err := m.Generate("my-key")
	require.NoError(t, err)
	require.NotEmpty(t, key)
	require.NotNil(t, rec)
	assert.Equal(t, "my-key", rec.Name)
	assert.Len(t, rec.KeyPrefix, PrefixLen)
	assert.Len(t, rec.Fingerprint, 8)
	assert.Equal(t, key[:PrefixLen], rec.KeyPrefix)
	assert.False(t, rec.Revoked())

	// Verify
	got, err := m.Verify(key)
	require.NoError(t, err)
	assert.Equal(t, rec.ID, got.ID)

	// List
	list := m.List()
	require.Len(t, list, 1)
	assert.Equal(t, rec.ID, list[0].ID)

	// Revoke
	require.NoError(t, m.Revoke(rec.ID))
	assert.True(t, rec.Revoked())
}

func TestManager_RejectsRevoked(t *testing.T) {
	m := newTestManager(t)
	key, rec, err := m.Generate("k1")
	require.NoError(t, err)
	require.NoError(t, m.Revoke(rec.ID))
	got, err := m.Verify(key)
	assert.ErrorIs(t, err, ErrRevoked)
	assert.Equal(t, rec.ID, got.ID) // record is still returned for audit
}

func TestManager_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apikeys.json")

	m1, err := NewManager(path)
	require.NoError(t, err)
	key, rec, err := m1.Generate("persisted")
	require.NoError(t, err)
	require.NoError(t, m1.Persist())

	// Fresh instance loads from disk.
	m2, err := NewManager(path)
	require.NoError(t, err)
	got, err := m2.Verify(key)
	require.NoError(t, err)
	assert.Equal(t, rec.ID, got.ID)
	assert.Equal(t, "persisted", got.Name)

	// Original plaintext still verifies after reload.
	list := m2.List()
	require.Len(t, list, 1)
}

func TestManager_VerifyUnknown(t *testing.T) {
	m := newTestManager(t)
	_, err := m.Verify("deadbeefcafebabe")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestManager_VerifyShortKey(t *testing.T) {
	m := newTestManager(t)
	_, err := m.Verify("short")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestManager_RevokeUnknown(t *testing.T) {
	m := newTestManager(t)
	err := m.Revoke("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestManager_GetUnknown(t *testing.T) {
	m := newTestManager(t)
	_, err := m.Get("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestManager_SetAccountScope(t *testing.T) {
	m := newTestManager(t)
	_, rec, err := m.Generate("k")
	require.NoError(t, err)
	require.NoError(t, m.SetAccountScope(rec.ID, "acct-1", "read"))
	got, err := m.Get(rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "acct-1", got.AccountID)
	assert.Equal(t, "read", got.Scope)
}

func TestManager_SetAccountScopeUnknown(t *testing.T) {
	m := newTestManager(t)
	err := m.SetAccountScope("nonexistent", "acct", "read")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestManager_PersistEmptyPath(t *testing.T) {
	m, err := NewManager("")
	require.NoError(t, err)
	_, _, err = m.Generate("k")
	require.NoError(t, err)
	require.NoError(t, m.Persist()) // no-op
}

func TestManager_PersistReloadsRevokedState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apikeys.json")
	m1, err := NewManager(path)
	require.NoError(t, err)
	key, rec, err := m1.Generate("k")
	require.NoError(t, err)
	require.NoError(t, m1.Revoke(rec.ID))
	require.NoError(t, m1.Persist())

	m2, err := NewManager(path)
	require.NoError(t, err)
	_, err = m2.Verify(key)
	assert.ErrorIs(t, err, ErrRevoked)
}

func TestManager_TwoKeysSamePrefix(t *testing.T) {
	// Different keys with potentially colliding prefixes still verify
	// to the correct record (constant-time hash compare disambiguates).
	m := newTestManager(t)
	key1, rec1, err := m.Generate("a")
	require.NoError(t, err)
	key2, rec2, err := m.Generate("b")
	require.NoError(t, err)

	got1, err := m.Verify(key1)
	require.NoError(t, err)
	assert.Equal(t, rec1.ID, got1.ID)

	got2, err := m.Verify(key2)
	require.NoError(t, err)
	assert.Equal(t, rec2.ID, got2.ID)

	// Wrong key (even with same prefix) must not verify to either record.
	// Synthesize a wrong key with key1's prefix but a different tail.
	wrong := key1[:PrefixLen] + "0000000000000000000000000000000000000000000000000000000000000000"
	_, err = m.Verify(wrong)
	assert.ErrorIs(t, err, ErrNotFound)
}

// Ensure errors are wrapped properly for errors.Is chains.
func TestManager_ErrorsAreSentinels(t *testing.T) {
	m := newTestManager(t)
	_, _, _ = m.Generate("k")
	_, err := m.Verify("deadbeefcafebabe")
	assert.True(t, errors.Is(err, ErrNotFound))
}
