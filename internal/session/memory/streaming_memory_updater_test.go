// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStreamingMemoryUpdaterConfig(t *testing.T) {
	t.Parallel()
	c := NewStreamingMemoryUpdaterConfig()
	assert.Equal(t, 8, c.MaxOperationsPerUpdate)
	assert.Equal(t, 10.0, c.MaxWaitSeconds)
	assert.Equal(t, 1.0, c.TimerCheckIntervalSeconds)
	assert.False(t, c.TraceConsole)
}

func TestStreamingMemoryUpdaterConfig_Validate(t *testing.T) {
	t.Parallel()
	c := NewStreamingMemoryUpdaterConfig()
	assert.NoError(t, c.Validate())
	c2 := StreamingMemoryUpdaterConfig{}
	assert.Error(t, c2.Validate())
	c3 := StreamingMemoryUpdaterConfig{MaxOperationsPerUpdate: 1, MaxWaitSeconds: 1, TimerCheckIntervalSeconds: 1}
	assert.NoError(t, c3.Validate())
}

func TestStreamingMemoryUpdaterKey(t *testing.T) {
	t.Parallel()
	k1 := StreamingMemoryUpdaterKey{AccountID: "a1", UserID: "u1"}
	k2 := StreamingMemoryUpdaterKey{AccountID: "a1", UserID: "u1"}
	k3 := StreamingMemoryUpdaterKey{AccountID: "a2", UserID: "u2"}
	assert.Equal(t, k1, k2)
	assert.NotEqual(t, k1, k3)
}

func TestMemoryMergeGroupKey(t *testing.T) {
	t.Parallel()
	k1 := MemoryMergeGroupKey{PeerID: "p1", MemoryType: "profiles"}
	k2 := MemoryMergeGroupKey{PeerID: "p1", MemoryType: "profiles"}
	assert.Equal(t, k1, k2)
}

func TestNewStreamingMemoryUpdater(t *testing.T) {
	t.Parallel()
	u := NewStreamingMemoryUpdater(nil, nil, NewStreamingMemoryUpdaterConfig())
	assert.NotNil(t, u)
	assert.False(t, u.Closed())
}

func TestStreamingMemoryUpdater_LastResult(t *testing.T) {
	t.Parallel()
	u := NewStreamingMemoryUpdater(nil, nil, NewStreamingMemoryUpdaterConfig())
	assert.Nil(t, u.LastResult())
}

func TestStreamingMemoryUpdater_GetBufferedOperationCount(t *testing.T) {
	t.Parallel()
	u := NewStreamingMemoryUpdater(nil, nil, NewStreamingMemoryUpdaterConfig())
	assert.Equal(t, 0, u.GetBufferedOperationCount())
}

func TestStreamingMemoryUpdater_Close_Empty(t *testing.T) {
	t.Parallel()
	u := NewStreamingMemoryUpdater(nil, nil, NewStreamingMemoryUpdaterConfig())
	out := u.Close()
	// Close with no pending batches returns nil.
	assert.Nil(t, out)
	assert.True(t, u.Closed())
}

func TestStreamingMemoryUpdater_Close_Idempotent(t *testing.T) {
	t.Parallel()
	u := NewStreamingMemoryUpdater(nil, nil, NewStreamingMemoryUpdaterConfig())
	u.Close()
	out := u.Close()
	assert.Nil(t, out)
}

func TestStreamingMemoryUpdater_Submit_NilCtx(t *testing.T) {
	t.Parallel()
	u := NewStreamingMemoryUpdater(nil, nil, NewStreamingMemoryUpdaterConfig())
	_, err := u.Submit(context.Background(), MemoryUpdateRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ctx is required")
}

func TestStreamingMemoryUpdater_Submit_Closed(t *testing.T) {
	t.Parallel()
	u := NewStreamingMemoryUpdater(nil, nil, NewStreamingMemoryUpdaterConfig())
	u.Close()
	_, err := u.Submit(context.Background(), MemoryUpdateRequest{Ctx: &IdentityContext{UserID: "u"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

func TestStreamingMemoryUpdater_ApplyOperations_ImplementsInterface(t *testing.T) {
	t.Parallel()
	// Compile-time assertion that *StreamingMemoryUpdater implements MemoryUpdater.
	var _ MemoryUpdater = (*StreamingMemoryUpdater)(nil)
}

func TestOperationCount(t *testing.T) {
	t.Parallel()
	ops := ResolvedOperations{
		UpsertOperations:   []ResolvedOperation{{}},
		DeleteFileContents: []MemoryFile{{}},
	}
	assert.Equal(t, 2, OperationCount(ops))
}

func TestMergeLinkLists_Empty(t *testing.T) {
	t.Parallel()
	out := MergeLinkLists(nil)
	assert.Empty(t, out)
}

func TestMergeLinkLists_Deduplicates(t *testing.T) {
	t.Parallel()
	links1 := []StoredLink{
		{FromURI: "a", ToURI: "b", LinkType: "related_to", Weight: 0.5, Description: "short"},
	}
	links2 := []StoredLink{
		{FromURI: "a", ToURI: "b", LinkType: "related_to", Weight: 0.8, Description: "longer description"},
	}
	out := MergeLinkLists(links1, links2)
	require.Len(t, out, 1)
	assert.Equal(t, 0.8, out[0].Weight)
	assert.Equal(t, "longer description", out[0].Description)
}

func TestMergeGroupKeyLabel(t *testing.T) {
	t.Parallel()
	k := MemoryMergeGroupKey{PeerID: "p1", MemoryType: "profiles"}
	assert.Equal(t, "peer=p1,memory_type=profiles", MergeGroupKeyLabel(k))
	k2 := MemoryMergeGroupKey{}
	assert.Equal(t, "peer=self,memory_type=unknown", MergeGroupKeyLabel(k2))
}

func TestPeerIDFromURI(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "peer1", PeerIDFromURI("viking://user/u/peers/peer1/memories/profiles/p.md"))
	assert.Equal(t, "", PeerIDFromURI("viking://user/u/memories/profiles/p.md"))
	assert.Equal(t, "", PeerIDFromURI(""))
}

func TestPeerIDForOperation(t *testing.T) {
	t.Parallel()
	op := ResolvedOperation{
		MemoryFields: map[string]any{"peer_id": "peer1"},
		URIs:         []string{"viking://user/u/peers/peer1/memories/profiles/p.md"},
	}
	assert.Equal(t, "peer1", PeerIDForOperation(op))
}

func TestPeerIDForOperation_SelfScope(t *testing.T) {
	t.Parallel()
	op := ResolvedOperation{
		MemoryFields: map[string]any{},
		URIs:         []string{"viking://user/u/memories/profiles/p.md"},
	}
	assert.Equal(t, "", PeerIDForOperation(op))
}

func TestPeerIDForMemoryFile(t *testing.T) {
	t.Parallel()
	mf := MemoryFile{
		URI:         "viking://user/u/peers/peer1/memories/profiles/p.md",
		ExtraFields: map[string]any{},
	}
	assert.Equal(t, "peer1", PeerIDForMemoryFile(mf))
}

func TestCloneOperationForURI(t *testing.T) {
	t.Parallel()
	op := ResolvedOperation{
		MemoryType:   "profiles",
		MemoryFields: map[string]any{"name": "test"},
		URIs:         []string{"uri1", "uri2"},
	}
	clone := CloneOperationForURI(op, "uri2")
	assert.Equal(t, []string{"uri2"}, clone.URIs)
	assert.Equal(t, "test", clone.MemoryFields["name"])
}

func TestSplitLinksForAppendOnlyOps(t *testing.T) {
	t.Parallel()
	links := []StoredLink{
		{FromURI: "append1", ToURI: "merge1", LinkType: "related_to"},
		{FromURI: "append1", ToURI: "append2", LinkType: "related_to"},
		{FromURI: "merge1", ToURI: "merge2", LinkType: "related_to"},
	}
	appendOps := []ResolvedOperation{{URIs: []string{"append1", "append2"}}}
	mergeOps := []ResolvedOperation{{URIs: []string{"merge1", "merge2"}}}
	appendLinks, mergeLinks := SplitLinksForAppendOnlyOps(links, appendOps, mergeOps)
	assert.Len(t, appendLinks, 1)
	assert.Len(t, mergeLinks, 2)
}

func TestCombineStreamingMemoryResults_Empty(t *testing.T) {
	t.Parallel()
	out := CombineStreamingMemoryResults(nil, 0)
	assert.Equal(t, 0, out.RequestCount)
	assert.NotNil(t, out.ApplyResult)
}

func TestCombineStreamingMemoryResults_Single(t *testing.T) {
	t.Parallel()
	r := &StreamingMemoryUpdateResult{
		Operations:   ResolvedOperations{},
		ApplyResult:  &MemoryUpdateResult{},
		RequestCount: 1,
		Metadata:     map[string]any{"flush_reason": "test"},
	}
	out := CombineStreamingMemoryResults([]*StreamingMemoryUpdateResult{r}, 0)
	assert.Equal(t, 1, out.RequestCount)
	assert.Equal(t, "test", out.Metadata["flush_reason"])
}

func TestCombineStreamingMemoryResults_Multiple(t *testing.T) {
	t.Parallel()
	r1 := &StreamingMemoryUpdateResult{
		Operations:   ResolvedOperations{UpsertOperations: []ResolvedOperation{{}}},
		ApplyResult:  &MemoryUpdateResult{WrittenURIs: []string{"uri1"}},
		RequestCount: 1,
		Metadata:     map[string]any{"flush_reason": "a"},
	}
	r2 := &StreamingMemoryUpdateResult{
		Operations:   ResolvedOperations{UpsertOperations: []ResolvedOperation{{}}},
		ApplyResult:  &MemoryUpdateResult{WrittenURIs: []string{"uri2"}},
		RequestCount: 2,
		Metadata:     map[string]any{"flush_reason": "b"},
	}
	out := CombineStreamingMemoryResults([]*StreamingMemoryUpdateResult{r1, r2}, 0)
	assert.Equal(t, 3, out.RequestCount)
	assert.Equal(t, "a+b", out.Metadata["flush_reason"])
	assert.Len(t, out.Operations.UpsertOperations, 2)
	assert.Len(t, out.ApplyResult.WrittenURIs, 2)
}

func TestClassifyMemoryMergeMode_Empty(t *testing.T) {
	t.Parallel()
	fast, reason := ClassifyMemoryMergeMode(nil, MemoryTypeSchema{})
	assert.True(t, fast)
	assert.Equal(t, "empty_batch", reason)
}

func TestClassifyMemoryMergeMode_AddOnly(t *testing.T) {
	t.Parallel()
	ops := []ResolvedOperation{{MemoryType: "events"}}
	schema := MemoryTypeSchema{MemoryType: "events", OperationMode: "add_only"}
	fast, reason := ClassifyMemoryMergeMode(ops, schema)
	assert.True(t, fast)
	assert.Equal(t, "add_only", reason)
}

func TestClassifyMemoryMergeMode_SingleNewFile(t *testing.T) {
	t.Parallel()
	ops := []ResolvedOperation{{
		MemoryType:   "profiles",
		MemoryFields: map[string]any{"name": "test"},
		URIs:         []string{"viking://user/u/memories/profiles/test.md"},
	}}
	schema := MemoryTypeSchema{MemoryType: "profiles"}
	fast, reason := ClassifyMemoryMergeMode(ops, schema)
	assert.True(t, fast)
	// A single new file with unique URI matches "unique_new_files" first.
	assert.Equal(t, "unique_new_files", reason)
}

func TestIsCrossExtractionGroup_SingleSource(t *testing.T) {
	t.Parallel()
	ops := []ResolvedOperation{
		{MemoryFields: map[string]any{"source_extraction_id": "id1"}},
		{MemoryFields: map[string]any{"source_extraction_id": "id1"}},
	}
	assert.False(t, IsCrossExtractionGroup(ops))
}

func TestIsCrossExtractionGroup_MultiSource(t *testing.T) {
	t.Parallel()
	ops := []ResolvedOperation{
		{MemoryFields: map[string]any{"source_extraction_id": "id1"}},
		{MemoryFields: map[string]any{"source_extraction_id": "id2"}},
	}
	assert.True(t, IsCrossExtractionGroup(ops))
}

func TestSourceExtractionIDForOperation(t *testing.T) {
	t.Parallel()
	op := ResolvedOperation{
		Source:        &MemoryOperationSource{ExtractionID: "id1"},
		MemoryFields:  map[string]any{},
	}
	assert.Equal(t, "id1", SourceExtractionIDForOperation(op))
}

func TestSourceExtractionIDForOperation_FromFields(t *testing.T) {
	t.Parallel()
	op := ResolvedOperation{
		MemoryFields: map[string]any{"source_extraction_id": "id2"},
	}
	assert.Equal(t, "id2", SourceExtractionIDForOperation(op))
}

func TestOptionalStr(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", optionalStr(nil))
	assert.Equal(t, "hello", optionalStr("hello"))
	assert.Equal(t, "123", optionalStr(123))
}

func TestOrDefault(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "def", orDefault("", "def"))
	assert.Equal(t, "val", orDefault("val", "def"))
}

func TestUniqueStrings(t *testing.T) {
	t.Parallel()
	in := []string{"a", "b", "a", "c", "b"}
	out := uniqueStrings(in)
	assert.Len(t, out, 3)
}

func TestMemoryOperationSourceFromRequest_Nil(t *testing.T) {
	t.Parallel()
	req := MemoryUpdateRequest{}
	out := MemoryOperationSourceFromRequest(req)
	assert.Nil(t, out)
}

func TestMemoryOperationSourceFromRequest_WithExtractionID(t *testing.T) {
	t.Parallel()
	req := MemoryUpdateRequest{
		Metadata: map[string]any{
			"source_extraction_id": "id1",
			"session_id":          "s1",
			"trace_id":            "t1",
		},
	}
	out := MemoryOperationSourceFromRequest(req)
	require.NotNil(t, out)
	assert.Equal(t, "id1", out.ExtractionID)
	assert.Equal(t, "s1", out.SessionID)
	assert.Equal(t, "t1", out.TraceID)
}

func TestAttachSourceToRequestOperations(t *testing.T) {
	t.Parallel()
	req := &MemoryUpdateRequest{
		Metadata: map[string]any{"source_extraction_id": "id1"},
		Operations: ResolvedOperations{
			UpsertOperations: []ResolvedOperation{
				{MemoryFields: map[string]any{}},
			},
		},
	}
	AttachSourceToRequestOperations(req)
	assert.Equal(t, "id1", req.Operations.UpsertOperations[0].MemoryFields["source_extraction_id"])
}

func TestFilterValidLinks_Empty(t *testing.T) {
	t.Parallel()
	out, err := FilterValidLinks(context.Background(), nil, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestFilterValidLinks_NilFS(t *testing.T) {
	t.Parallel()
	links := []StoredLink{
		{FromURI: "uri1", ToURI: "uri2", LinkType: "related_to"},
	}
	// With nil FS and no upserts, the endpoints are not found.
	out, err := FilterValidLinks(context.Background(), links, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestFilterValidLinks_WithUpserts(t *testing.T) {
	t.Parallel()
	links := []StoredLink{
		{FromURI: "uri1", ToURI: "uri2", LinkType: "related_to"},
	}
	upserts := []ResolvedOperation{{URIs: []string{"uri1", "uri2"}}}
	out, err := FilterValidLinks(context.Background(), links, upserts, nil, nil, nil)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestScopeMemoryUpdateResultToSubmitter_EmptyScope(t *testing.T) {
	t.Parallel()
	result := StreamingMemoryUpdateResult{
		Operations:   ResolvedOperations{},
		ApplyResult:  &MemoryUpdateResult{},
		RequestCount: 2,
		Metadata:     map[string]any{"operation_count": 2},
	}
	request := MemoryUpdateRequest{}
	out := ScopeMemoryUpdateResultToSubmitter(result, request)
	// Empty scope returns the original result.
	assert.Equal(t, 2, out.RequestCount)
}

func TestSplitRequestByMergeGroup_Empty(t *testing.T) {
	t.Parallel()
	req := MemoryUpdateRequest{}
	out := SplitRequestByMergeGroup(req)
	assert.Empty(t, out)
}

func TestSplitRequestByMergeGroup_WithOps(t *testing.T) {
	t.Parallel()
	req := MemoryUpdateRequest{
		Operations: ResolvedOperations{
			UpsertOperations: []ResolvedOperation{
				{
					MemoryType:   "profiles",
					MemoryFields: map[string]any{"name": "test"},
					URIs:         []string{"viking://user/u/memories/profiles/test.md"},
				},
			},
		},
	}
	out := SplitRequestByMergeGroup(req)
	require.Len(t, out, 1)
	assert.Equal(t, "profiles", out[0].Key.MemoryType)
}

func TestCombinedRequestMessages(t *testing.T) {
	t.Parallel()
	items := []MemoryUpdateRequest{
		{Messages: []Message{NewTextMessage("1", "user", "", "2026-01-01T00:00:00Z", []MessagePart{NewTextOnlyPart("a")})}},
		{Messages: []Message{NewTextMessage("2", "user", "", "2026-01-01T00:00:00Z", []MessagePart{NewTextOnlyPart("b")})}},
	}
	out := CombinedRequestMessages(items)
	assert.Len(t, out, 2)
}

func TestMergeOutputLanguageFromMessages_Empty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", MergeOutputLanguageFromMessages(nil))
}

func TestMergeOutputLanguageFromMessages_WithText(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewTextMessage("1", "user", "", "2026-01-01T00:00:00Z",
			[]MessagePart{NewTextOnlyPart("hello world")}),
	}
	out := MergeOutputLanguageFromMessages(msgs)
	assert.NotEqual(t, "", out)
}

func TestMemoryFileToDeletePatch(t *testing.T) {
	t.Parallel()
	mf := MemoryFile{
		URI:         "viking://user/u/memories/profiles/p.md",
		MemoryType:  "profiles",
		Content:     "old content",
		ExtraFields: map[string]any{"name": "test"},
	}
	schema := MemoryTypeSchema{MemoryType: "profiles"}
	patch := MemoryFileToDeletePatch(mf, schema)
	require.NotNil(t, patch.BeforeFile)
	assert.Equal(t, "old content", patch.BeforeFile.Content)
	assert.Equal(t, "", patch.AfterFile.Content)
	assert.Equal(t, "viking://user/u/memories/profiles/p.md", patch.AfterFile.URI)
}

func TestEnforceMergeGroupPeerID(t *testing.T) {
	t.Parallel()
	ops := []ResolvedOperation{
		{MemoryType: "profiles", MemoryFields: map[string]any{}},
	}
	registry := NewMemoryTypeRegistry()
	schema, _ := registry.Get("profiles")
	if schema.MemoryType != "" {
		EnforceMergeGroupPeerID(ops, "peer1", "profiles", registry, nil)
		// Should set peer_id in the operation.
		if schema.PeerEnabled {
			assert.Equal(t, "peer1", ops[0].MemoryFields["peer_id"])
		}
	}
}
