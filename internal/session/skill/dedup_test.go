// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package skill

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	sessionmemory "github.com/saker-ai/ctxhub/internal/session/memory"
)

func ptrString(s string) *string { return &s }

func ptrMemoryFile(content string) *sessionmemory.MemoryFile {
	return &sessionmemory.MemoryFile{URI: "", Content: content}
}

func TestDedupOperations_RemovesDuplicateSessionSkills(t *testing.T) {
	t.Parallel()
	in := ResolvedOperations{
		UpsertOperations: []ResolvedOperation{
			{MemoryType: "session_skills", MemoryFields: map[string]any{
				"skill_name": "search",
				"content":    "alpha beta",
			}},
			{MemoryType: "session_skills", MemoryFields: map[string]any{
				"skill_name": "search",
				"content":    "alpha   beta", // whitespace-normalized duplicate
			}},
			{MemoryType: "session_skills", MemoryFields: map[string]any{
				"skill_name": "other",
				"content":    "gamma",
			}},
		},
	}

	out := DedupOperations(in)
	assert.Len(t, out.UpsertOperations, 2, "first dupe should be dropped")
	assert.Equal(t, "alpha beta", out.UpsertOperations[0].MemoryFields["content"])
	assert.Equal(t, "gamma", out.UpsertOperations[1].MemoryFields["content"])
}

func TestDedupOperations_PassesThroughNonSessionSkills(t *testing.T) {
	t.Parallel()
	in := ResolvedOperations{
		UpsertOperations: []ResolvedOperation{
			{MemoryType: "memory", MemoryFields: map[string]any{"content": "alpha"}},
			{MemoryType: "entity", MemoryFields: map[string]any{"content": "alpha"}},
		},
	}
	out := DedupOperations(in)
	assert.Len(t, out.UpsertOperations, 2, "non-session_skills ops must not be deduped")
}

func TestDedupOperations_PassesThroughUpdateOps(t *testing.T) {
	t.Parallel()
	prev := ptrMemoryFile("previous body")
	in := ResolvedOperations{
		UpsertOperations: []ResolvedOperation{
			{MemoryType: "session_skills", OldMemoryFileContent: prev, MemoryFields: map[string]any{
				"content": "alpha",
			}},
			{MemoryType: "session_skills", OldMemoryFileContent: prev, MemoryFields: map[string]any{
				"content": "alpha",
			}},
		},
	}
	out := DedupOperations(in)
	assert.Len(t, out.UpsertOperations, 2, "ops with OldMemoryFileContent are updates, not dedupable")
}

func TestDedupOperations_EmptyBodyPassesThrough(t *testing.T) {
	t.Parallel()
	in := ResolvedOperations{
		UpsertOperations: []ResolvedOperation{
			{MemoryType: "session_skills", MemoryFields: map[string]any{
				"skill_name": "x",
				"content":    "",
			}},
			{MemoryType: "session_skills", MemoryFields: map[string]any{
				"skill_name": "x",
				"content":    "   ",
			}},
		},
	}
	out := DedupOperations(in)
	assert.Len(t, out.UpsertOperations, 2, "empty-body ops should not be deduped")
}

func TestDedupOperations_NoDuplicatesReturnsInputUnchanged(t *testing.T) {
	t.Parallel()
	in := ResolvedOperations{
		UpsertOperations: []ResolvedOperation{
			{MemoryType: "session_skills", MemoryFields: map[string]any{"content": "a"}},
			{MemoryType: "session_skills", MemoryFields: map[string]any{"content": "b"}},
		},
		Errors: []string{"boom"},
	}
	out := DedupOperations(in)
	// No duplicates → returns input as-is (same pointer or equal value).
	assert.Equal(t, in, out)
}

func TestDedupOperations_PreservesDeleteAndErrors(t *testing.T) {
	t.Parallel()
	in := ResolvedOperations{
		UpsertOperations: []ResolvedOperation{
			{MemoryType: "session_skills", MemoryFields: map[string]any{"content": "alpha"}},
			{MemoryType: "session_skills", MemoryFields: map[string]any{"content": "alpha"}},
		},
		DeleteFileContents: []sessionmemory.MemoryFile{
			{URI: "a.md"},
			{URI: "b.md"},
		},
		Errors: []string{"err1"},
	}
	out := DedupOperations(in)
	assert.Len(t, out.UpsertOperations, 1)
	assert.Equal(t, []sessionmemory.MemoryFile{{URI: "a.md"}, {URI: "b.md"}}, out.DeleteFileContents)
	assert.Equal(t, []string{"err1"}, out.Errors)
}

func TestDedupOperations_MergeBlockBody(t *testing.T) {
	t.Parallel()
	// Merge-op shape: {blocks: [{search:"", replace:"body"}]}.
	in := ResolvedOperations{
		UpsertOperations: []ResolvedOperation{
			{MemoryType: "session_skills", MemoryFields: map[string]any{
				"content": map[string]any{
					"blocks": []any{
						map[string]any{"search": "", "replace": "hello world"},
					},
				},
			}},
			{MemoryType: "session_skills", MemoryFields: map[string]any{
				"content": map[string]any{
					"blocks": []any{
						map[string]any{"search": "  ", "replace": "hello   world"},
					},
				},
			}},
		},
	}
	out := DedupOperations(in)
	assert.Len(t, out.UpsertOperations, 1, "merge-block bodies should normalize to same signature")
}

func TestDedupOperations_MergeBlockWithSearchNonEmptyNotDeduped(t *testing.T) {
	t.Parallel()
	// When search is non-empty, the op is a patch, not a full body — skip dedup.
	in := ResolvedOperations{
		UpsertOperations: []ResolvedOperation{
			{MemoryType: "session_skills", MemoryFields: map[string]any{
				"content": map[string]any{
					"blocks": []any{
						map[string]any{"search": "old", "replace": "new"},
					},
				},
			}},
			{MemoryType: "session_skills", MemoryFields: map[string]any{
				"content": map[string]any{
					"blocks": []any{
						map[string]any{"search": "old", "replace": "new"},
					},
				},
			}},
		},
	}
	out := DedupOperations(in)
	assert.Len(t, out.UpsertOperations, 2, "patch ops (search!=empty) should not be deduped")
}

func TestResolvedOperations_HasErrors(t *testing.T) {
	t.Parallel()
	assert.False(t, ResolvedOperations{}.HasErrors())
	assert.False(t, ResolvedOperations{Errors: nil}.HasErrors())
	assert.True(t, ResolvedOperations{Errors: []string{"x"}}.HasErrors())
}

func TestSkillName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "search", SkillName(ResolvedOperation{
		MemoryFields: map[string]any{"skill_name": "search"},
	}))
	assert.Equal(t, "<unknown>", SkillName(ResolvedOperation{
		MemoryFields: map[string]any{"skill_name": "  "},
	}))
	assert.Equal(t, "<unknown>", SkillName(ResolvedOperation{
		MemoryFields: map[string]any{},
	}))
	// Non-string skill_name falls back to unknown.
	assert.Equal(t, "<unknown>", SkillName(ResolvedOperation{
		MemoryFields: map[string]any{"skill_name": 42},
	}))
}

func TestNormalizeText(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"hello", "hello"},
		{"  hello  ", "hello"},
		{"hello   world", "hello world"},
		{"hello\tworld\n", "hello world"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, normalizeText(c.in))
	}
}

func TestExtractFullBody_String(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "alpha", extractFullBody("alpha"))
	assert.Equal(t, "", extractFullBody(""))
	assert.Equal(t, "", extractFullBody(123))
}

func TestExtractFullBody_MergeBlock(t *testing.T) {
	t.Parallel()
	// Valid merge block: single block, empty search, non-empty replace.
	body := map[string]any{
		"blocks": []any{
			map[string]any{"search": "", "replace": "real body"},
		},
	}
	assert.Equal(t, "real body", extractFullBody(body))

	// Multiple blocks → not a full body.
	multi := map[string]any{
		"blocks": []any{
			map[string]any{"search": "", "replace": "a"},
			map[string]any{"search": "", "replace": "b"},
		},
	}
	assert.Equal(t, "", extractFullBody(multi))

	// No blocks key.
	assert.Equal(t, "", extractFullBody(map[string]any{}))

	// Block not a map.
	assert.Equal(t, "", extractFullBody(map[string]any{
		"blocks": []any{"not a map"},
	}))
}

func TestBuildSessionSkillSignature(t *testing.T) {
	t.Parallel()
	// Happy path: session_skills + content + no old file content.
	sig := buildSessionSkillSignature(ResolvedOperation{
		MemoryType:   "session_skills",
		MemoryFields: map[string]any{"content": "hello world"},
	})
	assert.True(t, strings.HasPrefix(sig, "session_skill_body:"))
	assert.Contains(t, sig, "hello world")

	// Not session_skills.
	assert.Equal(t, "", buildSessionSkillSignature(ResolvedOperation{
		MemoryType:   "memory",
		MemoryFields: map[string]any{"content": "hello"},
	}))

	// Has old file content (update op).
	old := ptrMemoryFile("old")
	assert.Equal(t, "", buildSessionSkillSignature(ResolvedOperation{
		MemoryType:           "session_skills",
		OldMemoryFileContent: old,
		MemoryFields:         map[string]any{"content": "hello"},
	}))

	// Empty content.
	assert.Equal(t, "", buildSessionSkillSignature(ResolvedOperation{
		MemoryType:   "session_skills",
		MemoryFields: map[string]any{"content": ""},
	}))
}
