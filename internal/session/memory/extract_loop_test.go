// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVLMResponse_HasToolCalls(t *testing.T) {
	t.Parallel()
	r := &VLMResponse{}
	assert.False(t, r.HasToolCalls())
	r.ToolCalls = []ToolCall{{ID: "1", Name: "read"}}
	assert.True(t, r.HasToolCalls())
}

func TestVLMResponse_HasToolCalls_Nil(t *testing.T) {
	t.Parallel()
	var r *VLMResponse
	assert.False(t, r.HasToolCalls())
}

func TestLooksLikeCannedRefusal_Chinese(t *testing.T) {
	t.Parallel()
	assert.True(t, LooksLikeCannedRefusal("抱歉，我无法回答这个问题"))
	assert.True(t, LooksLikeCannedRefusal("很遗憾未能找到相关结果"))
}

func TestLooksLikeCannedRefusal_English(t *testing.T) {
	t.Parallel()
	assert.True(t, LooksLikeCannedRefusal("I can't answer that"))
	assert.True(t, LooksLikeCannedRefusal("Sorry, I cannot help with that"))
}

func TestLooksLikeCannedRefusal_NotRefusal(t *testing.T) {
	t.Parallel()
	assert.False(t, LooksLikeCannedRefusal("Here is the JSON output"))
	assert.False(t, LooksLikeCannedRefusal(""))
}

func TestPreviewText(t *testing.T) {
	t.Parallel()
	out := PreviewText("hello world", 5)
	assert.Equal(t, "he...", out)
}

func TestPreviewText_NoTruncation(t *testing.T) {
	t.Parallel()
	out := PreviewText("hello", 10)
	assert.Equal(t, "hello", out)
}

func TestPreviewText_DefaultLimit(t *testing.T) {
	t.Parallel()
	out := PreviewText("hello", 0)
	assert.Equal(t, "hello", out)
}

func TestPreviewText_CollapsesWhitespace(t *testing.T) {
	t.Parallel()
	out := PreviewText("hello   world", 100)
	assert.Equal(t, "hello world", out)
}

func TestExtractLoop_NewExtractLoop_DefaultMaxIterations(t *testing.T) {
	t.Parallel()
	l := NewExtractLoop(nil, nil, "", 0, nil, nil, nil, false)
	assert.Equal(t, 3, l.MaxIterations)
}

func TestExtractLoop_NewExtractLoop_ExplicitMaxIterations(t *testing.T) {
	t.Parallel()
	l := NewExtractLoop(nil, nil, "", 5, nil, nil, nil, false)
	assert.Equal(t, 5, l.MaxIterations)
}

func TestExtractLoop_Run_NilProvider(t *testing.T) {
	t.Parallel()
	l := NewExtractLoop(nil, nil, "", 0, nil, nil, nil, false)
	_, _, err := l.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context provider is required")
}

func TestToItemList_Slice(t *testing.T) {
	t.Parallel()
	in := []any{
		map[string]any{"a": "1"},
		map[string]any{"b": "2"},
	}
	out := toItemList(in)
	assert.Len(t, out, 2)
}

func TestToItemList_Map(t *testing.T) {
	t.Parallel()
	in := map[string]any{"a": "1"}
	out := toItemList(in)
	require.Len(t, out, 1)
}

func TestToItemList_Nil(t *testing.T) {
	t.Parallel()
	assert.Empty(t, toItemList(nil))
}

func TestToDeleteIDs(t *testing.T) {
	t.Parallel()
	in := []any{
		map[string]any{"delete_page_id": 5, "replacement_page_id": 10},
	}
	out := toDeleteIDs(in)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].DeletePageID)
	assert.Equal(t, 5, *out[0].DeletePageID)
	require.NotNil(t, out[0].ReplacementPageID)
	assert.Equal(t, 10, *out[0].ReplacementPageID)
}

func TestToDeleteIDs_NilReplacement(t *testing.T) {
	t.Parallel()
	in := []any{
		map[string]any{"delete_page_id": 5, "replacement_page_id": nil},
	}
	out := toDeleteIDs(in)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].DeletePageID)
	assert.Nil(t, out[0].ReplacementPageID)
}

func TestAppendIfMissing(t *testing.T) {
	t.Parallel()
	list := []string{"a", "b"}
	out := appendIfMissing(list, "a")
	assert.Equal(t, []string{"a", "b"}, out)
	out = appendIfMissing(list, "c")
	assert.Equal(t, []string{"a", "b", "c"}, out)
}

func TestBuildToolSchemas(t *testing.T) {
	t.Parallel()
	l := &ExtractLoop{}
	out := l.buildToolSchemas([]string{"read"})
	// May be empty if the "read" tool is not registered; just verify no panic.
	_ = out
}

func TestBuildToolSchemas_Empty(t *testing.T) {
	t.Parallel()
	l := &ExtractLoop{}
	out := l.buildToolSchemas(nil)
	assert.Nil(t, out)
}

func TestBuildFinalOperationsInstruction(t *testing.T) {
	t.Parallel()
	l := &ExtractLoop{expectedFields: []string{"profiles"}}
	out := l.buildFinalOperationsInstruction()
	assert.Contains(t, out, "JSON")
	assert.Contains(t, out, "profiles")
}
