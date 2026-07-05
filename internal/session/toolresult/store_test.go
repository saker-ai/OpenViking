// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package toolresult

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

func newTestStore(t *testing.T) (*Store, *memfs.MemFS) {
	t.Helper()
	fs := memfs.New("test")
	return NewStore(fs, "sessions/sess-1", "sess-1"), fs
}

func TestStore_Write_CreatesFiles(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	res, err := store.Write(ctx, WriteOptions{
		Content:      `{"name":"alice","age":30}`,
		ToolID:       "search_1",
		ToolName:     "search",
		MessageID:    "msg-1",
		UserID:       "user-1",
		PreviewChars: 100,
		MimeType:     "application/json",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.NotEmpty(t, res.ToolResultID)
	assert.Contains(t, res.ToolResultID, "tr_search_1_")
	assert.Equal(t, "sessions/sess-1/tool-results/"+res.ToolResultID, res.StorageURI)
	assert.Equal(t, res.StorageURI+"/output.txt", res.OutputURI)
	assert.Equal(t, res.StorageURI+"/metadata.json", res.MetadataURI)
	assert.Equal(t, KindJSON, res.Synopsis.Kind)
	assert.Equal(t, "search", res.Metadata["tool_name"])
	assert.Equal(t, "msg-1", res.Metadata["message_id"])
	assert.Equal(t, "user-1", res.Metadata["user_id"])
}

func TestStore_Write_IdempotentForSameContent(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	opts := WriteOptions{
		Content:  "hello world",
		ToolID:   "t1",
		ToolName: "tool",
	}
	first, err := store.Write(ctx, opts)
	require.NoError(t, err)

	// Write same content again — should return the same record without
	// rewriting files.
	second, err := store.Write(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, first.ToolResultID, second.ToolResultID)
	assert.Equal(t, first.Metadata["sha256"], second.Metadata["sha256"])
}

func TestStore_Write_DifferentContentDifferentID(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	a, err := store.Write(ctx, WriteOptions{Content: "aaa", ToolID: "t", ToolName: "n"})
	require.NoError(t, err)
	b, err := store.Write(ctx, WriteOptions{Content: "bbb", ToolID: "t", ToolName: "n"})
	require.NoError(t, err)
	assert.NotEqual(t, a.ToolResultID, b.ToolResultID)
}

func TestStore_ReadMetadata_NotFound(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.ReadMetadata(context.Background(), "tr_missing_abc")
	assert.Error(t, err)
}

func TestStore_ReadMetadata_InvalidID(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.ReadMetadata(context.Background(), "with/slash")
	assert.Error(t, err)
}

func TestStore_Read_Full(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	content := "line one\nline two\nline three\n"
	res, err := store.Write(ctx, WriteOptions{Content: content, ToolID: "t", ToolName: "n"})
	require.NoError(t, err)

	got, err := store.Read(ctx, res.ToolResultID, ReadOptions{Offset: 0, Limit: -1, IncludeMetadata: true})
	require.NoError(t, err)
	assert.Equal(t, content, got.Content)
	assert.Equal(t, len(content), got.TotalChars)
	assert.False(t, got.HasMore)
	assert.Equal(t, "t", got.Metadata["tool_id"])
}

func TestStore_Read_Paged(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	content := "0123456789"
	res, err := store.Write(ctx, WriteOptions{Content: content, ToolID: "t", ToolName: "n"})
	require.NoError(t, err)

	got, err := store.Read(ctx, res.ToolResultID, ReadOptions{Offset: 3, Limit: 4})
	require.NoError(t, err)
	assert.Equal(t, "3456", got.Content)
	assert.Equal(t, 3, got.Offset)
	assert.Equal(t, 4, got.Limit)
	assert.Equal(t, 10, got.TotalChars)
	assert.True(t, got.HasMore)
}

func TestStore_Read_OffsetBeyondEnd(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	res, err := store.Write(ctx, WriteOptions{Content: "abc", ToolID: "t", ToolName: "n"})
	require.NoError(t, err)

	got, err := store.Read(ctx, res.ToolResultID, ReadOptions{Offset: 100, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, "", got.Content)
	assert.False(t, got.HasMore)
}

func TestStore_Read_NegativeOffset(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.Read(context.Background(), "x", ReadOptions{Offset: -1, Limit: 10})
	assert.Error(t, err)
}

func TestStore_Read_NegativeLimit(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.Read(context.Background(), "x", ReadOptions{Offset: 0, Limit: -2})
	assert.Error(t, err)
}

func TestStore_Search_FindsMatches(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	content := "foo bar foo baz foo"
	res, err := store.Write(ctx, WriteOptions{Content: content, ToolID: "t", ToolName: "n"})
	require.NoError(t, err)

	got, err := store.Search(ctx, res.ToolResultID, SearchOptions{Query: "foo", Limit: 10, ContextChars: 3})
	require.NoError(t, err)
	require.Len(t, got.Matches, 3)
	assert.Equal(t, 0, got.Matches[0].Offset)
	assert.Equal(t, "foo", got.Matches[0].Snippet[:3])
	assert.Equal(t, 8, got.Matches[1].Offset)
	assert.Equal(t, 16, got.Matches[2].Offset)
}

func TestStore_Search_LimitBounded(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	content := strings.Repeat("x", 100)
	res, err := store.Write(ctx, WriteOptions{Content: content, ToolID: "t", ToolName: "n"})
	require.NoError(t, err)

	got, err := store.Search(ctx, res.ToolResultID, SearchOptions{Query: "x", Limit: 5, ContextChars: 0})
	require.NoError(t, err)
	assert.Len(t, got.Matches, 5)
}

func TestStore_Search_EmptyQuery(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.Search(context.Background(), "x", SearchOptions{Query: "", Limit: 10})
	assert.Error(t, err)
}

func TestStore_Search_ZeroLimit(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.Search(context.Background(), "x", SearchOptions{Query: "q", Limit: 0})
	assert.Error(t, err)
}

func TestStore_List_Empty(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	got, err := store.List(context.Background(), ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, got.ToolResults)
}

func TestStore_List_FiltersByToolName(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	_, err := store.Write(ctx, WriteOptions{Content: "a", ToolID: "1", ToolName: "search"})
	require.NoError(t, err)
	_, err = store.Write(ctx, WriteOptions{Content: "b", ToolID: "2", ToolName: "calc"})
	require.NoError(t, err)

	got, err := store.List(ctx, ListOptions{ToolName: "search", Limit: 10})
	require.NoError(t, err)
	require.Len(t, got.ToolResults, 1)
	assert.Equal(t, "search", got.ToolResults[0]["tool_name"])
}

func TestStore_List_ZeroLimit(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.List(context.Background(), ListOptions{Limit: 0})
	assert.Error(t, err)
}

func TestStore_MakePreview_EndToEnd(t *testing.T) {
	t.Parallel()
	content := `{"name":"alice","age":30}`
	stub := MakePreview(content, 100, "ref-1", "search", SHA256Text(content), "too big", "application/json")
	assert.Contains(t, stub, "[OpenViking tool result externalized]")
	assert.Contains(t, stub, "tool_name: search")
	assert.Contains(t, stub, "kind: json")
	assert.Contains(t, stub, "ref: ref-1")
	assert.Contains(t, stub, "reason: too big")
}

func TestStore_ValidateToolResultID_Integration(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Write a valid record, then read it back via ReadMetadata to confirm
	// the ID validation logic accepts the IDs we generate.
	res, err := store.Write(ctx, WriteOptions{Content: "x", ToolID: "t", ToolName: "n"})
	require.NoError(t, err)

	meta, err := store.ReadMetadata(ctx, res.ToolResultID)
	require.NoError(t, err)
	assert.Equal(t, res.ToolResultID, meta["tool_result_id"])
}

func TestStore_Read_NotFoundReturnsError(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.Read(context.Background(), "tr_unknown", ReadOptions{Limit: -1})
	assert.Error(t, err)
	// domain.ErrNotFound wrapping — error message should mention NotFound or the wrapped form.
	lower := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(lower, "notfound") || strings.Contains(lower, "not found") ||
			strings.Contains(lower, domain.ErrNotFound.Error()),
		"expected NotFound-flavoured error, got: %v", err)
}
