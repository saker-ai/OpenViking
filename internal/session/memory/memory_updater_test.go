// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryUpdaterInterface(t *testing.T) {
	t.Parallel()
	// Compile-time assertion that memoryUpdater implements MemoryUpdater.
	var _ MemoryUpdater = (*memoryUpdater)(nil)
	var _ MemoryUpdater = (*StreamingMemoryUpdater)(nil)
}

func TestNewMemoryUpdater(t *testing.T) {
	t.Parallel()
	u := NewMemoryUpdater()
	assert.NotNil(t, u)
}

func TestNewMemoryUpdaterWithRegistry(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	u := NewMemoryUpdaterWithRegistry(r, nil)
	assert.NotNil(t, u)
}

func TestMemoryUpdater_SetRegistry(t *testing.T) {
	t.Parallel()
	u := NewMemoryUpdater()
	r := NewMemoryTypeRegistry()
	u.SetRegistry(r)
	// Should not panic.
}

func TestMemoryUpdater_SetVikingFS(t *testing.T) {
	t.Parallel()
	u := NewMemoryUpdater()
	u.SetVikingFS(nil)
	// Should not panic.
}

func TestMemoryTypeFromURI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		uri  string
		want string
	}{
		{"viking://user/u/memories/profiles/p.md", "profiles"},
		{"viking://user/u/memories/preferences/x.md", "preferences"},
		{"viking://user/u/foo/bar.md", ""},
		{"", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, MemoryTypeFromURI(c.uri))
	}
}

func TestMemoryUpdater_ApplyOperations_NilFS(t *testing.T) {
	t.Parallel()
	u := NewMemoryUpdater()
	_, err := u.ApplyOperations(context.Background(), ResolvedOperations{}, nil, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MemoryFS not configured")
}

func TestMemoryUpdateResult_AddWritten(t *testing.T) {
	t.Parallel()
	r := &MemoryUpdateResult{}
	r.AddWritten("viking://user/u/memories/profiles/a.md")
	assert.Len(t, r.WrittenURIs, 1)
}

func TestMemoryUpdateResult_AddEdited(t *testing.T) {
	t.Parallel()
	r := &MemoryUpdateResult{}
	r.AddEdited("viking://user/u/memories/profiles/a.md")
	assert.Len(t, r.EditedURIs, 1)
}

func TestMemoryUpdateResult_AddDeleted(t *testing.T) {
	t.Parallel()
	r := &MemoryUpdateResult{}
	r.AddDeleted("viking://user/u/memories/profiles/a.md")
	assert.Len(t, r.DeletedURIs, 1)
}

func TestMemoryUpdateResult_AddError(t *testing.T) {
	t.Parallel()
	r := &MemoryUpdateResult{}
	r.AddError("viking://user/u/memories/profiles/a.md", assert.AnError)
	assert.Len(t, r.Errors, 1)
}

func TestMemoryUpdateResult_HasChanges(t *testing.T) {
	t.Parallel()
	r := &MemoryUpdateResult{}
	assert.False(t, r.HasChanges())
	r.AddWritten("uri")
	assert.True(t, r.HasChanges())
}

func TestMemoryUpdateResult_Summary(t *testing.T) {
	t.Parallel()
	r := &MemoryUpdateResult{}
	r.AddWritten("uri1")
	r.AddEdited("uri2")
	r.AddDeleted("uri3")
	s := r.Summary()
	assert.Contains(t, s, "Written: 1")
	assert.Contains(t, s, "Edited: 1")
	assert.Contains(t, s, "Deleted: 1")
}

func TestRemapStoredLinks(t *testing.T) {
	t.Parallel()
	links := []StoredLink{
		{FromURI: "old1", ToURI: "old2", LinkType: "related_to", Weight: 0.5},
	}
	remap := map[string]string{"old1": "new1", "old2": "new2"}
	out := RemapStoredLinks(links, remap)
	require.Len(t, out, 1)
	assert.Equal(t, "new1", out[0].FromURI)
	assert.Equal(t, "new2", out[0].ToURI)
}

func TestRemapStoredLinks_NoRemap(t *testing.T) {
	t.Parallel()
	links := []StoredLink{
		{FromURI: "uri1", ToURI: "uri2", LinkType: "related_to", Weight: 0.5},
	}
	out := RemapStoredLinks(links, nil)
	require.Len(t, out, 1)
	assert.Equal(t, "uri1", out[0].FromURI)
}

func TestRemapLinkDict(t *testing.T) {
	t.Parallel()
	link := map[string]any{
		"from_uri": "old1",
		"to_uri":   "old2",
	}
	remap := map[string]string{"old1": "new1", "old2": "new2"}
	out := RemapLinkDict(link, remap)
	assert.Equal(t, "new1", out["from_uri"])
	assert.Equal(t, "new2", out["to_uri"])
}
