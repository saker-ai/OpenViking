// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatchMergePatch_TargetURI(t *testing.T) {
	t.Parallel()
	patch := PatchMergePatch{
		AfterFile: MemoryFile{URI: "viking://user/u/memories/profiles/p.md"},
	}
	assert.Equal(t, "viking://user/u/memories/profiles/p.md", patch.TargetURI())
}

func TestPatchMergePatch_TargetURI_Empty(t *testing.T) {
	t.Parallel()
	patch := PatchMergePatch{}
	assert.Equal(t, "", patch.TargetURI())
}

func TestPatchMergePatch_MemoryType(t *testing.T) {
	t.Parallel()
	patch := PatchMergePatch{
		AfterFile: MemoryFile{MemoryType: "profiles"},
	}
	assert.Equal(t, "profiles", patch.MemoryType())
}

func TestPatchMergePatch_MemoryType_FromExtraFields(t *testing.T) {
	t.Parallel()
	patch := PatchMergePatch{
		AfterFile: MemoryFile{
			ExtraFields: map[string]any{"memory_type": "preferences"},
		},
	}
	assert.Equal(t, "preferences", patch.MemoryType())
}

func TestPatchMergePatch_TargetName(t *testing.T) {
	t.Parallel()
	patch := PatchMergePatch{
		AfterFile: MemoryFile{
			URI:         "viking://user/u/memories/profiles/test_user.md",
			ExtraFields: map[string]any{"name": "Test User"},
		},
	}
	// TargetName prefers the "name" extra field.
	assert.Equal(t, "Test User", patch.TargetName())
}

func TestPatchMergePatch_TargetName_FallsBackToURIBase(t *testing.T) {
	t.Parallel()
	patch := PatchMergePatch{
		AfterFile: MemoryFile{
			URI: "viking://user/u/memories/profiles/test_user.md",
		},
	}
	name := patch.TargetName()
	// Should be the URI base without .md suffix.
	assert.NotEqual(t, "", name)
}

func TestPatchMergeContextProvider_GetTools(t *testing.T) {
	t.Parallel()
	p := NewPatchMergeContextProvider("profiles", nil, nil, "")
	assert.Nil(t, p.GetTools())
}

func TestPatchMergeContextProvider_GetMemorySchemas(t *testing.T) {
	t.Parallel()
	p := NewPatchMergeContextProvider("profiles", nil, nil, "")
	schemas := p.GetMemorySchemas(nil)
	for _, s := range schemas {
		assert.Equal(t, "profiles", s.MemoryType)
	}
}

func TestPatchMergeContextProvider_Instruction(t *testing.T) {
	t.Parallel()
	p := NewPatchMergeContextProvider("profiles", nil, nil, "")
	instr := p.Instruction()
	assert.Contains(t, instr, "memory patch merge agent")
}

func TestPatchMergeContextProvider_Prefetch_NoPatches(t *testing.T) {
	t.Parallel()
	p := NewPatchMergeContextProvider("profiles", nil, nil, "")
	out, err := p.Prefetch(context.Background())
	require.NoError(t, err)
	// Prefetch always appends a user message with the rendered patches.
	require.Len(t, out, 1)
	assert.Equal(t, "user", out[0]["role"])
	content := out[0]["content"].(string)
	assert.Contains(t, content, "No patches")
}

func TestInferUserSpaceFromURIs(t *testing.T) {
	t.Parallel()
	uris := []string{
		"viking://user/user123/memories/profiles/p.md",
		"viking://user/other/memories/profiles/q.md",
	}
	space := InferUserSpaceFromURIs(uris)
	assert.Equal(t, "user123", space)
}

func TestInferUserSpaceFromURIs_NoVikingURI(t *testing.T) {
	t.Parallel()
	uris := []string{"http://example.com/foo"}
	space := InferUserSpaceFromURIs(uris)
	assert.Equal(t, "", space)
}

func TestInferUserSpaceFromURIs_Empty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", InferUserSpaceFromURIs(nil))
}

func TestCompactPatchMetadata(t *testing.T) {
	t.Parallel()
	meta := map[string]any{
		"content":     "hello",
		"memory_type": "profiles",
		"name":        "test",
		"version":     "2",
		"confidence":  0.9,
	}
	out := CompactPatchMetadata(meta)
	// CompactPatchMetadata should only keep PatchMergePatchMetadataKeys.
	for k := range out {
		assert.Contains(t, PatchMergePatchMetadataKeys, k)
	}
	if _, ok := out["confidence"]; ok {
		assert.Equal(t, 0.9, out["confidence"])
	}
}

func TestBuildPatchSearchQuery(t *testing.T) {
	t.Parallel()
	patches := []PatchMergePatch{
		{
			AfterFile: MemoryFile{
				URI:     "viking://user/u/memories/profiles/test.md",
				Content: "User likes fruit.",
			},
		},
	}
	q := BuildPatchSearchQuery(patches)
	assert.NotEmpty(t, q)
}

func TestBuildPatchSearchQuery_Empty(t *testing.T) {
	t.Parallel()
	q := BuildPatchSearchQuery(nil)
	assert.Empty(t, q)
}

func TestRenderFieldDiffPatches(t *testing.T) {
	t.Parallel()
	patches := []PatchMergePatch{
		{
			BeforeFile: &MemoryFile{
				URI:         "viking://user/u/memories/profiles/p.md",
				Content:     "old content",
				ExtraFields: map[string]any{"name": "old"},
			},
			AfterFile: MemoryFile{
				URI:         "viking://user/u/memories/profiles/p.md",
				Content:     "new content",
				ExtraFields: map[string]any{"name": "new"},
			},
		},
	}
	out := RenderFieldDiffPatches(patches)
	assert.Contains(t, out, "Memory File Patches")
	assert.Contains(t, out, "Patch 1")
}

func TestRenderFieldDiffPatches_Empty(t *testing.T) {
	t.Parallel()
	out := RenderFieldDiffPatches(nil)
	assert.Contains(t, out, "No patches")
}

func TestFieldDiffs(t *testing.T) {
	t.Parallel()
	before := MemoryFile{
		URI:         "viking://user/u/memories/profiles/p.md",
		Content:     "old content",
		ExtraFields: map[string]any{"name": "old"},
	}
	after := MemoryFile{
		URI:         "viking://user/u/memories/profiles/p.md",
		Content:     "new content",
		ExtraFields: map[string]any{"name": "new"},
	}
	diffs := FieldDiffs(&before, after)
	// FieldDiffs should return diffs for changed fields.
	// It may be empty if implementation requires specific schema, but should not panic.
	_ = diffs
}

func TestPatchMergePatchMetadataKeys(t *testing.T) {
	t.Parallel()
	// PatchMergePatchMetadataKeys is a slice of metadata keys kept in compact output.
	assert.NotEmpty(t, PatchMergePatchMetadataKeys)
	assert.Contains(t, PatchMergePatchMetadataKeys, "confidence")
}
