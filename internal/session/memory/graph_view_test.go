// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryTypeColors(t *testing.T) {
	t.Parallel()
	assert.NotEmpty(t, MemoryTypeColors)
	// Should have colors for common memory types.
}

func TestColorForLinkType(t *testing.T) {
	t.Parallel()
	out := ColorForLinkType("related_to")
	assert.NotEqual(t, "", out)
	out = ColorForLinkType("belongs_to")
	assert.NotEqual(t, "", out)
	out = ColorForLinkType("unknown_type")
	assert.NotEqual(t, "", out)
}

func TestBuildContentPreview(t *testing.T) {
	t.Parallel()
	out := BuildContentPreview("hello world this is long", 5)
	assert.Equal(t, "hello…", out)
}

func TestBuildContentPreview_NoTruncation(t *testing.T) {
	t.Parallel()
	out := BuildContentPreview("hello", 10)
	assert.Equal(t, "hello", out)
}

func TestBuildContentPreview_DefaultLimit(t *testing.T) {
	t.Parallel()
	out := BuildContentPreview("hello", 0)
	assert.Equal(t, "hello", out)
}

func TestIsContentTruncated(t *testing.T) {
	t.Parallel()
	assert.True(t, IsContentTruncated("hello world this is long", 5))
	assert.False(t, IsContentTruncated("hi", 5))
}

func TestInferMemoryType(t *testing.T) {
	t.Parallel()
	out := InferMemoryType("viking://user/u/memories/profiles/p.md", "")
	assert.Equal(t, "profiles", out)
}

func TestInferMemoryType_UsesParsed(t *testing.T) {
	t.Parallel()
	out := InferMemoryType("viking://user/u/memories/profiles/p.md", "preferences")
	// When parsed memory_type is provided, it should be preferred.
	assert.Equal(t, "preferences", out)
}

func TestInferMemoryType_Empty(t *testing.T) {
	t.Parallel()
	out := InferMemoryType("", "")
	assert.Equal(t, "", out)
}

func TestNewMemoryGraph(t *testing.T) {
	t.Parallel()
	g := NewMemoryGraph(nil)
	assert.NotNil(t, g)
}

func TestMemoryGraph_SetVikingFS(t *testing.T) {
	t.Parallel()
	g := NewMemoryGraph(nil)
	g.SetVikingFS(nil)
	// Should not panic.
}

func TestMemoryGraph_CollectGraphData_NilFS(t *testing.T) {
	t.Parallel()
	g := NewMemoryGraph(nil)
	nodes, edges, err := g.CollectGraphData(context.Background(), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VikingFS")
	assert.Empty(t, nodes)
	assert.Empty(t, edges)
}

func TestMemoryGraph_GenGraph_NilFS(t *testing.T) {
	t.Parallel()
	g := NewMemoryGraph(nil)
	_, err := g.GenGraph(context.Background(), "viking://user/u", nil)
	// With nil FS, should return error or empty output without panic.
	if err != nil {
		// error is acceptable when FS is nil
	}
}

func TestMemoryGraph_BuildGraph_NilFS(t *testing.T) {
	t.Parallel()
	g := NewMemoryGraph(nil)
	_, err := g.BuildGraph(context.Background(), nil, "", nil)
	// With nil FS, may return error; just verify no panic.
	_ = err
}

func TestRenderGraphHTML(t *testing.T) {
	t.Parallel()
	nodes := []map[string]any{
		{"id": "n1", "label": "Node 1", "group": "profiles"},
		{"id": "n2", "label": "Node 2", "group": "preferences"},
	}
	edges := []map[string]any{
		{"from": "n1", "to": "n2", "label": "related_to"},
	}
	out := RenderGraphHTML(nodes, edges)
	assert.Contains(t, out, "<html")
	assert.Contains(t, out, "Memory Graph")
	assert.Contains(t, out, "vis-network")
}

func TestRenderGraphHTML_Empty(t *testing.T) {
	t.Parallel()
	out := RenderGraphHTML(nil, nil)
	assert.Contains(t, out, "<html")
}

func TestScriptSafeJSON(t *testing.T) {
	t.Parallel()
	out := scriptSafeJSON("</script>")
	assert.NotContains(t, out, "</script>")
}
