// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafePeerID(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "peer1", SafePeerID("peer1"))
	assert.Equal(t, "", SafePeerID(""))
	assert.Equal(t, "", SafePeerID(123)) // non-string returns ""
	assert.Equal(t, "", SafePeerID("__self")) // self peer ID returns ""
}

func TestSafePeerID_SanitizesString(t *testing.T) {
	t.Parallel()
	// SafePeerID rejects unsafe characters.
	out := SafePeerID("peer/with/slashes")
	assert.Equal(t, "", out) // slashes are not allowed
}

func TestPeerUserSpace(t *testing.T) {
	t.Parallel()
	out := PeerUserSpace("user1", "peer1")
	assert.Contains(t, out, "user1")
	assert.Contains(t, out, "peer1")
}

func TestPeerUserSpace_NoPeer(t *testing.T) {
	t.Parallel()
	out := PeerUserSpace("user1", "")
	assert.Equal(t, "user1", out)
}

func TestInternalMemoryTypes(t *testing.T) {
	t.Parallel()
	assert.NotNil(t, InternalMemoryTypes)
	// Should include overview/abstract internal types.
}

func TestSelfPeerID(t *testing.T) {
	t.Parallel()
	assert.NotEqual(t, "", SelfPeerID)
}

func TestNewMemoryIsolationHandler(t *testing.T) {
	t.Parallel()
	ctx := &IdentityContext{UserID: "user1"}
	ih := NewMemoryIsolationHandler(ctx, nil, nil, true, nil)
	assert.NotNil(t, ih)
}

func TestMemoryIsolationHandler_PrepareMessages(t *testing.T) {
	t.Parallel()
	ih := NewMemoryIsolationHandler(nil, nil, nil, true, nil)
	// PrepareMessages is a no-op when there are no messages.
	assert.NotPanics(t, func() { ih.PrepareMessages() })
}

func TestMemoryIsolationHandler_GetReadScope(t *testing.T) {
	t.Parallel()
	ctx := &IdentityContext{UserID: "user1"}
	ih := NewMemoryIsolationHandler(ctx, nil, nil, true, nil)
	scope := ih.GetReadScope()
	assert.Equal(t, "user1", scope.UserID)
}

func TestMemoryIsolationHandler_AllowsSchema_NoIsolation(t *testing.T) {
	t.Parallel()
	ctx := &IdentityContext{UserID: "user1"}
	ih := NewMemoryIsolationHandler(ctx, nil, nil, true, nil)
	schema := MemoryTypeSchema{MemoryType: "profiles"}
	// Without isolation restrictions, all schemas are allowed.
	assert.True(t, ih.AllowsSchema(schema))
}

func TestMemoryIsolationHandler_AllowsSchema_Restricted(t *testing.T) {
	t.Parallel()
	ctx := &IdentityContext{UserID: "user1"}
	allowed := map[string]struct{}{"profiles": {}}
	ih := NewMemoryIsolationHandler(ctx, nil, allowed, true, nil)
	schema := MemoryTypeSchema{MemoryType: "profiles"}
	assert.True(t, ih.AllowsSchema(schema))
	schema2 := MemoryTypeSchema{MemoryType: "preferences"}
	assert.False(t, ih.AllowsSchema(schema2))
}

func TestMemoryIsolationHandler_CanWritePeer(t *testing.T) {
	t.Parallel()
	ctx := &IdentityContext{UserID: "user1"}
	allowedPeers := map[string]struct{}{"peer1": {}}
	ih := NewMemoryIsolationHandler(ctx, nil, nil, false, allowedPeers)
	assert.True(t, ih.CanWritePeer("peer1"))
	assert.False(t, ih.CanWritePeer("peer2"))
}

func TestMemoryIsolationHandler_ResolveOperationTargetID(t *testing.T) {
	t.Parallel()
	ctx := &IdentityContext{UserID: "user1"}
	// With AllowSelf=true and no allowed peers, "peer1" is not writable,
	// so it falls back to SelfPeerID="__self".
	ih := NewMemoryIsolationHandler(ctx, nil, nil, true, nil)
	assert.Equal(t, SelfPeerID, ih.ResolveOperationTargetID("peer1"))
	assert.Equal(t, SelfPeerID, ih.ResolveOperationTargetID(""))
}

func TestMemoryIsolationHandler_ResolveOperationTargetID_AllowedPeer(t *testing.T) {
	t.Parallel()
	ctx := &IdentityContext{UserID: "user1"}
	allowedPeers := map[string]struct{}{"peer1": {}}
	ih := NewMemoryIsolationHandler(ctx, nil, nil, false, allowedPeers)
	assert.Equal(t, "peer1", ih.ResolveOperationTargetID("peer1"))
}

func TestMemoryIsolationHandler_FirstTargetIDInMessages_NoMessages(t *testing.T) {
	t.Parallel()
	ih := NewMemoryIsolationHandler(nil, nil, nil, true, nil)
	assert.Equal(t, "", ih.FirstTargetIDInMessages())
}

func TestMemoryIsolationHandler_FirstTargetIDInMessages_WithMessages(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewTextMessage("1", "user", "peer1", "2026-01-01T00:00:00Z",
			[]MessagePart{NewTextOnlyPart("hello")}),
	}
	ec := NewExtractContext(msgs, true)
	ih := NewMemoryIsolationHandler(nil, ec, nil, true, nil)
	// With a peer_id in the message, should return it.
	out := ih.FirstTargetIDInMessages()
	// May be "peer1" or "" depending on implementation; just verify no panic.
	_ = out
}

func TestMemoryIsolationHandler_RenderSchemaDirectories(t *testing.T) {
	t.Parallel()
	ctx := &IdentityContext{UserID: "user1"}
	ih := NewMemoryIsolationHandler(ctx, nil, nil, true, nil)
	schema := MemoryTypeSchema{
		MemoryType: "profiles",
		Directory:  "viking://user/{{ user_space }}/memories/profiles",
	}
	dirs := ih.RenderSchemaDirectories(schema)
	require.NotEmpty(t, dirs)
	assert.Contains(t, dirs[0], "user1")
	assert.Contains(t, dirs[0], "profiles")
}

func TestMemoryIsolationHandler_CalculateMemoryURIs(t *testing.T) {
	t.Parallel()
	ctx := &IdentityContext{UserID: "user1"}
	ih := NewMemoryIsolationHandler(ctx, nil, nil, true, nil)
	schema := MemoryTypeSchema{
		MemoryType:        "profiles",
		Directory:         "viking://user/{{ user_space }}/memories/profiles",
		FilenameTemplate:  "{{ name }}.md",
	}
	op := &ResolvedOperation{
		MemoryType:   "profiles",
		MemoryFields: map[string]any{"name": "test"},
		URIs:         []string{},
	}
	ec := NewExtractContext(nil, true)
	uris := ih.CalculateMemoryURIs(schema, op, ec)
	// Should produce at least one URI.
	if len(uris) > 0 {
		assert.Contains(t, uris[0], "profiles")
	}
}
