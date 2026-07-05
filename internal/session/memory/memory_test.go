// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecutionMemoryTypes(t *testing.T) {
	t.Parallel()
	assert.True(t, IsExecutionMemoryType("trajectories"))
	assert.True(t, IsExecutionMemoryType("experiences"))
	assert.False(t, IsExecutionMemoryType("skills"))
	assert.False(t, IsExecutionMemoryType(""))
}

func TestFieldTypeToSchema(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ft   FieldType
		want string
	}{
		{FieldTypeString, "string"},
		{FieldTypeInt64, "integer"},
		{FieldTypeFloat32, "number"},
		{FieldTypeBool, "boolean"},
		{FieldType("unknown"), "string"}, // default
	}
	for _, c := range cases {
		assert.Equal(t, c.want, FieldTypeToSchema(c.ft))
	}
}

func TestNormalizeLinkType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", "related_to"},
		{"belongs_to", "belongs_to"},
		{"BELONGS_TO", "belongs_to"},
		{"belongs-to", "belongs_to"},
		{"belongs to", "belongs_to"},
		{"invalid link type", "invalid_link_type"}, // becomes 3-segment snake_case (valid)
		{"caused_by", "caused_by"},
		{"a_b_c", "a_b_c"}, // 3 segments allowed
		{"a_b_c_d", "related_to"}, // 4 segments not allowed
	}
	for _, c := range cases {
		assert.Equal(t, c.want, NormalizeLinkType(c.in), "input=%q", c.in)
	}
}

func TestNormalizeWeight(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0.5, NormalizeWeight("not a number"))
	assert.Equal(t, 0.0, NormalizeWeight(-1))
	assert.Equal(t, 1.0, NormalizeWeight(2.0))
	assert.Equal(t, 0.7, NormalizeWeight(0.7))
	assert.Equal(t, 0.5, NormalizeWeight(nil))
}

func TestStrPatchFirstReplace(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", StrPatch{}.FirstReplace())
	assert.Equal(t, "", StrPatch{Blocks: []SearchReplaceBlock{}}.FirstReplace())
	assert.Equal(t, "hello", StrPatch{Blocks: []SearchReplaceBlock{{Replace: "hello"}}}.FirstReplace())
}

func TestMemoryTypeSchema_FilenameHasVariables(t *testing.T) {
	t.Parallel()
	assert.True(t, MemoryTypeSchema{FilenameTemplate: "{{name}}.md"}.FilenameHasVariables())
	assert.False(t, MemoryTypeSchema{FilenameTemplate: "profile.md"}.FilenameHasVariables())
	assert.False(t, MemoryTypeSchema{FilenameTemplate: "{{name}.md"}.FilenameHasVariables()) // no closing }}
}

func TestResolvedOperation_IsEdit(t *testing.T) {
	t.Parallel()
	op := ResolvedOperation{}
	assert.False(t, op.IsEdit())
	op.OldMemoryFileContent = &MemoryFile{URI: "x.md"}
	assert.True(t, op.IsEdit())
}

func TestResolvedOperations_HasErrors(t *testing.T) {
	t.Parallel()
	assert.False(t, ResolvedOperations{}.HasErrors())
	assert.False(t, ResolvedOperations{Errors: nil}.HasErrors())
	assert.True(t, ResolvedOperations{Errors: []string{"x"}}.HasErrors())
}

func TestMemoryFile_FromParsed(t *testing.T) {
	t.Parallel()
	parsed := map[string]any{
		"content":     "hello",
		"memory_type": "skills",
		"version":     2,
		"links": []any{
			map[string]any{"to_uri": "x"},
		},
	}
	mf := FromParsed("viking://user/u/memories/x.md", parsed)
	assert.Equal(t, "hello", mf.Content)
	assert.Equal(t, "skills", mf.MemoryType)
	assert.Equal(t, 2, mf.ExtraFields["version"])
	require.Len(t, mf.Links, 1)
	assert.Equal(t, "x", mf.Links[0]["to_uri"])
}

func TestMemoryFile_ToMetadata(t *testing.T) {
	t.Parallel()
	mf := MemoryFile{
		Content:    "body",
		MemoryType: "skills",
		ExtraFields: map[string]any{
			"user_id": "u123", // filtered
			"name":    "search",
		},
		Links: []map[string]any{{"to_uri": "x"}},
	}
	md := mf.ToMetadata()
	assert.Equal(t, "body", md["content"])
	assert.Equal(t, "skills", md["memory_type"])
	assert.Equal(t, "search", md["name"])
	_, hasUserID := md["user_id"]
	assert.False(t, hasUserID)
	assert.Equal(t, 1, md["version"]) // default
	_, hasLinks := md["links"]
	assert.True(t, hasLinks)
}

func TestMemoryFile_PlainContent(t *testing.T) {
	t.Parallel()
	mf := MemoryFile{Content: "see [search](rel/path) for details"}
	assert.Equal(t, "see search for details", mf.PlainContent())
}

func TestStructuredMemoryOperations_IsEmpty(t *testing.T) {
	t.Parallel()
	assert.True(t, StructuredMemoryOperations{}.IsEmpty())
	assert.False(t, StructuredMemoryOperations{WriteURIs: []map[string]any{{}}}.IsEmpty())
	assert.False(t, StructuredMemoryOperations{EditURIs: []map[string]any{{}}}.IsEmpty())
	assert.False(t, StructuredMemoryOperations{DeleteIDs: []DeleteId{{}}}.IsEmpty())
}

func TestPatchOp_Apply(t *testing.T) {
	t.Parallel()
	// Non-string field: direct replace.
	op := NewPatchOp(FieldTypeInt64)
	assert.Equal(t, 42, op.Apply(1, 42))

	// String field with no original content: extract replace from patch.
	opStr := NewPatchOp(FieldTypeString)
	assert.Equal(t, "hello", opStr.Apply(nil, StrPatch{Blocks: []SearchReplaceBlock{{Search: "", Replace: "hello"}}}))

	// String field with original content: simple substring replace.
	assert.Equal(t, "world world", opStr.Apply("hello world", StrPatch{Blocks: []SearchReplaceBlock{{Search: "hello", Replace: "world"}}}))

	// Empty patch value preserves current.
	assert.Equal(t, "current", opStr.Apply("current", ""))
}

func TestReplaceOp_Apply(t *testing.T) {
	t.Parallel()
	op := NewReplaceOp()
	assert.Equal(t, "new", op.Apply("old", "new"))
	assert.Equal(t, "old", op.Apply("old", "")) // empty preserves
	assert.Equal(t, "old", op.Apply("old", nil))
}

func TestSumOp_Apply(t *testing.T) {
	t.Parallel()
	op := NewSumOp()
	assert.Equal(t, int64(5), op.Apply(2, 3))
	assert.Equal(t, 3, op.Apply(nil, 3))
	assert.Equal(t, 5, op.Apply(5, "")) // empty preserves
	assert.Equal(t, 5.5, op.Apply(2.0, 3.5))
}

func TestImmutableOp_Apply(t *testing.T) {
	t.Parallel()
	op := NewImmutableOp()
	assert.Equal(t, "v1", op.Apply(nil, "v1"))
	assert.Equal(t, "v1", op.Apply("v1", "v2")) // already set
}

func TestMergeOpFactory_Create(t *testing.T) {
	t.Parallel()
	f := NewMergeOpFactory()
	assert.Equal(t, MergeOpPatch, f.Create(MergeOpPatch, FieldTypeString).OpType())
	assert.Equal(t, MergeOpReplace, f.Create(MergeOpReplace, FieldTypeString).OpType())
	assert.Equal(t, MergeOpSum, f.Create(MergeOpSum, FieldTypeInt64).OpType())
	assert.Equal(t, MergeOpImmutable, f.Create(MergeOpImmutable, FieldTypeString).OpType())
	// Unknown op falls back to Patch.
	assert.Equal(t, MergeOpPatch, f.Create(MergeOp("bogus"), FieldTypeString).OpType())
}

func TestMergeOpFactory_FromField(t *testing.T) {
	t.Parallel()
	f := NewMergeOpFactory()
	op := f.FromField(MemoryField{Name: "count", FieldType: FieldTypeInt64, MergeOp: MergeOpSum})
	assert.Equal(t, MergeOpSum, op.OpType())
}

func TestMergeLinks(t *testing.T) {
	t.Parallel()
	existing := []map[string]any{
		{"from_uri": "a", "to_uri": "b", "match_text": "x", "weight": 0.5, "link_type": "related_to"},
	}
	incoming := []map[string]any{
		{"from_uri": "a", "to_uri": "b", "match_text": "x", "weight": 0.9, "link_type": "caused_by", "description": "new"},
		{"from_uri": "c", "to_uri": "d", "match_text": "y", "weight": 0.5},
	}
	merged := MergeLinks(existing, incoming)
	require.Len(t, merged, 2)
	// First link: max weight + latest link_type/description.
	assert.Equal(t, 0.9, merged[0]["weight"])
	assert.Equal(t, "caused_by", merged[0]["link_type"])
	assert.Equal(t, "new", merged[0]["description"])
	// Second link: new entry.
	assert.Equal(t, "c", merged[1]["from_uri"])
}

func TestApplyStrPatch_SimpleSubstring(t *testing.T) {
	t.Parallel()
	patch := StrPatch{Blocks: []SearchReplaceBlock{
		{Search: "hello", Replace: "world"},
	}}
	out, err := ApplyStrPatch("hello world", patch)
	require.NoError(t, err)
	assert.Equal(t, "world world", out)
}

func TestApplyStrPatch_MultipleBlocks(t *testing.T) {
	t.Parallel()
	patch := StrPatch{Blocks: []SearchReplaceBlock{
		{Search: "foo", Replace: "bar"},
		{Search: "baz", Replace: "qux"},
	}}
	out, err := ApplyStrPatch("foo baz", patch)
	require.NoError(t, err)
	assert.Equal(t, "bar qux", out)
}

func TestApplyStrPatch_EmptyBlocks(t *testing.T) {
	t.Parallel()
	out, err := ApplyStrPatch("hello", StrPatch{})
	require.NoError(t, err)
	assert.Equal(t, "hello", out)
}

func TestApplyStrPatch_NoMatchReturnsError(t *testing.T) {
	t.Parallel()
	patch := StrPatch{Blocks: []SearchReplaceBlock{
		{Search: "nonexistent", Replace: "x"},
	}}
	_, err := ApplyStrPatch("hello", patch)
	assert.Error(t, err)
}

func TestApplyStrPatch_IdenticalSearchReplace(t *testing.T) {
	t.Parallel()
	patch := StrPatch{Blocks: []SearchReplaceBlock{
		{Search: "same", Replace: "same"},
	}}
	out, err := ApplyStrPatch("same", patch)
	require.NoError(t, err)
	assert.Equal(t, "same", out)
}

func TestLevenshteinDistance(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, LevenshteinDistance("hello", "hello"))
	assert.Equal(t, 1, LevenshteinDistance("hello", "hallo"))
	assert.Equal(t, 3, LevenshteinDistance("kitten", "sitting"))
	assert.Equal(t, 5, LevenshteinDistance("hello", ""))
}

func TestGetSimilarity(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 1.0, GetSimilarity("hello", "hello"))
	assert.Equal(t, 0.0, GetSimilarity("hello", ""))
	// "hello" vs "hallo" differ by 1 char out of 5; similarity = 0.8.
	assert.InDelta(t, 0.8, GetSimilarity("hello", "hallo"), 0.01)
}

func TestLineNumbers(t *testing.T) {
	t.Parallel()
	content := "line1\nline2\nline3"
	numbered := AddLineNumbers(content, 1)
	assert.Equal(t, "1\tline1\n2\tline2\n3\tline3", numbered)
	assert.True(t, EveryLineHasLineNumbers(numbered))
	assert.Equal(t, 3, LineCount(content))
	assert.Equal(t, "line2\nline3", SliceContentLines(content, 1, -1))
	assert.Equal(t, "line2", SliceContentLines(content, 1, 1))
	assert.Equal(t, 1, ExtractStartLineNumber(numbered))
}

func TestStripLineNumbersContent(t *testing.T) {
	t.Parallel()
	numbered := "1\thello\n2\tworld"
	stripped := stripLineNumbersContent(numbered, false)
	assert.Equal(t, "hello\nworld", stripped)
}

func TestStripLinks(t *testing.T) {
	t.Parallel()
	content := "see [search](rel/path) and [external](https://example.com) and [anchor](#section)"
	out := StripLinks(content)
	assert.Equal(t, "see search and [external](https://example.com) and [anchor](#section)", out)
}

func TestStripAllLinks(t *testing.T) {
	t.Parallel()
	content := "see [search](rel/path) and [external](https://example.com)"
	out := StripAllLinks(content)
	assert.Equal(t, "see search and external", out)
}

func TestParseMemoryFileWithFields(t *testing.T) {
	t.Parallel()
	content := "body text\n\n<!-- MEMORY_FIELDS\n{\"version\": 2, \"name\": \"search\"}\n-->"
	parsed := ParseMemoryFileWithFields(content)
	assert.Equal(t, "body text", parsed["content"])
	assert.Equal(t, "search", parsed["name"])
	assert.Equal(t, float64(2), parsed["version"])
}

func TestParseMemoryFileWithFields_NoComment(t *testing.T) {
	t.Parallel()
	parsed := ParseMemoryFileWithFields("just text")
	assert.Equal(t, "just text", parsed["content"])
	_, hasVersion := parsed["version"]
	assert.False(t, hasVersion)
}

func TestMemoryFileReadAndWrite_RoundTrip(t *testing.T) {
	t.Parallel()
	mf := MemoryFile{
		URI:        "viking://user/u/memories/x.md",
		Content:    "hello world",
		MemoryType: "skills",
		ExtraFields: map[string]any{
			"version": 1,
			"name":    "search",
		},
	}
	serialized := MemoryFileWrite(mf, "")
	parsed := MemoryFileRead(serialized, mf.URI)
	assert.Equal(t, "hello world", parsed.Content)
	assert.Equal(t, "skills", parsed.MemoryType)
	assert.Equal(t, "search", parsed.ExtraFields["name"])
}

func TestNextMemoryVersion(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 1, NextMemoryVersion(nil))
	mf := MemoryFile{ExtraFields: map[string]any{"version": 3}}
	assert.Equal(t, 4, NextMemoryVersion(&mf))
}

func TestTruncateContent(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 2000)
	truncated := TruncateContent(long, 1000)
	assert.Less(t, len(truncated), len(long))
	assert.Contains(t, truncated, "truncated")
}

func TestGenerateURI(t *testing.T) {
	t.Parallel()
	mt := MemoryTypeSchema{
		Directory:        "viking://user/{{ user_space }}/memories",
		FilenameTemplate: "{{ name }}.md",
	}
	fields := map[string]any{"name": "search"}
	uri, err := GenerateURI(mt, fields, "user123")
	require.NoError(t, err)
	assert.Equal(t, "viking://user/user123/memories/search.md", uri)
}

func TestGenerateURI_MissingVariable(t *testing.T) {
	t.Parallel()
	mt := MemoryTypeSchema{
		Directory:        "viking://user/{{ user_space }}/memories",
		FilenameTemplate: "{{ name }}.md",
	}
	_, err := GenerateURI(mt, map[string]any{}, "user123")
	assert.Error(t, err)
}

func TestValidateURITemplate(t *testing.T) {
	t.Parallel()
	// Valid: template variables exist in fields.
	mt := MemoryTypeSchema{
		Directory:        "viking://user/{{ user_space }}/memories",
		FilenameTemplate: "{{ name }}.md",
		Fields:           []MemoryField{{Name: "name", FieldType: FieldTypeString}},
	}
	assert.True(t, ValidateURITemplate(mt))

	// Invalid: variable not in fields.
	mt2 := MemoryTypeSchema{
		FilenameTemplate: "{{ missing }}.md",
		Fields:           []MemoryField{{Name: "name"}},
	}
	assert.False(t, ValidateURITemplate(mt2))

	// Empty: invalid.
	assert.False(t, ValidateURITemplate(MemoryTypeSchema{}))
}

func TestIsURIAllowed(t *testing.T) {
	t.Parallel()
	dirs := []string{"viking://user/u/memories"}
	patterns := []string{"viking://user/u/skills/{{ name }}.md"}
	assert.True(t, IsURIAllowed("viking://user/u/memories/profile.md", dirs, patterns))
	assert.True(t, IsURIAllowed("viking://user/u/skills/search.md", dirs, patterns))
	assert.False(t, IsURIAllowed("viking://user/u/other/x.md", dirs, nil))
}

func TestPatternMatchesURI(t *testing.T) {
	t.Parallel()
	assert.True(t, patternMatchesURI("viking://user/u/skills/{{ name }}.md", "viking://user/u/skills/search.md"))
	assert.False(t, patternMatchesURI("viking://user/u/skills/{{ name }}.md", "viking://user/u/skills/search.txt"))
	assert.True(t, patternMatchesURI("viking://user/u/*/x.md", "viking://user/u/memories/x.md"))
}

func TestExtractJSONContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"prefix {\"a\": 1} suffix", "{\"a\": 1}"},
		{"[{\"a\": 1}] trailing", "[{\"a\": 1}]"},
		{"no json here", "no json here"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ExtractJSONContent(c.in), "input=%q", c.in)
	}
}

func TestParseJSONWithStability(t *testing.T) {
	t.Parallel()
	parsed, err := ParseJSONWithStability(`prefix {"a": 1, "b": "two"} suffix`)
	require.NoError(t, err)
	assert.Equal(t, float64(1), parsed["a"])
	assert.Equal(t, "two", parsed["b"])

	// List -> first item.
	parsed, err = ParseJSONWithStability(`[{"x": 1}]`)
	require.NoError(t, err)
	assert.Equal(t, float64(1), parsed["x"])

	// Empty.
	_, err = ParseJSONWithStability("")
	assert.Error(t, err)
}

func TestAddToolCallPairToMessages(t *testing.T) {
	t.Parallel()
	msgs := []map[string]any{}
	msgs = AddToolCallPairToMessages(msgs, "id1", "read", map[string]any{"uri": "x"}, map[string]any{"content": "y"})
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0]["role"])
	content, _ := msgs[0]["content"].(string)
	assert.Contains(t, content, "read")
	assert.Contains(t, content, "x")
}

func TestOptimizeSearchResult(t *testing.T) {
	t.Parallel()
	result := map[string]any{
		"memories": []any{
			map[string]any{"uri": "viking://user/u/memories/skills.md", "score": 0.9},
			map[string]any{"uri": "viking://user/u/memories/.abstract.md", "score": 0.5},
			map[string]any{"uri": "viking://user/u/memories/.overview.md", "score": 0.5},
		},
	}
	out := OptimizeSearchResult(result, 10).([]any)
	require.Len(t, out, 1)
	first := out[0].(map[string]any)
	assert.Equal(t, "viking://user/u/memories/skills.md", first["uri"])
}

func TestExtractErrorSummary(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "File not found", ExtractErrorSummary("File not found: x.md"))
	assert.Equal(t, "Permission denied", ExtractErrorSummary("Permission denied for x"))
	assert.Equal(t, "Timeout", ExtractErrorSummary("Timeout contacting server"))
	assert.Equal(t, "short error", ExtractErrorSummary("short error"))
	assert.Equal(t, strings.Repeat("a", 50), ExtractErrorSummary(strings.Repeat("a", 100)))
}

func TestFormatSize(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "500B", FormatSize(500))
	assert.Equal(t, "1.0K", FormatSize(1024))
	assert.Equal(t, "1.0M", FormatSize(1024*1024))
}

func TestResolveOutputLanguageFromText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "zh-CN", ResolveOutputLanguageFromText("你好世界", "en"))
	assert.Equal(t, "en", ResolveOutputLanguageFromText("hello world", "en"))
	assert.Equal(t, "ja", ResolveOutputLanguageFromText("こんにちは", "en"))
	assert.Equal(t, "ko", ResolveOutputLanguageFromText("안녕하세요", "en"))
	assert.Equal(t, "en", ResolveOutputLanguageFromText("", "en"))
}

func TestDetectLanguageFromConversation(t *testing.T) {
	t.Parallel()
	conv := "[user]: 你好\n[assistant]: hello"
	assert.Equal(t, "zh-CN", DetectLanguageFromConversation(conv, "en"))
}

func TestStripLanguageDetectionNoise(t *testing.T) {
	t.Parallel()
	text := "see viking://user/u/memories/x.md for details"
	out := stripLanguageDetectionNoise(text)
	assert.NotContains(t, out, "viking://")
}

// ----- registry tests -----

func TestRegistry_RegisterAndGet(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	mt := MemoryTypeSchema{MemoryType: "profile", Enabled: true}
	require.NoError(t, r.Register(mt))
	got, ok := r.Get("profile")
	require.True(t, ok)
	assert.Equal(t, "profile", got.MemoryType)
	_, ok = r.Get("missing")
	assert.False(t, ok)
}

func TestRegistry_RegisterDuplicateFails(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	mt := MemoryTypeSchema{MemoryType: "profile", Enabled: true}
	require.NoError(t, r.Register(mt))
	err := r.Register(mt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestRegistry_Replace(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	original := MemoryTypeSchema{MemoryType: "profile", Description: "v1", Enabled: true}
	require.NoError(t, r.Register(original))
	updated := MemoryTypeSchema{MemoryType: "profile", Description: "v2", Enabled: true}
	r.Replace(updated)
	got, _ := r.Get("profile")
	assert.Equal(t, "v2", got.Description)
}

func TestRegistry_ListAllFiltersDisabled(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "a", Enabled: true})
	_ = r.Register(MemoryTypeSchema{MemoryType: "b", Enabled: false})
	enabled := r.ListAll(false)
	assert.Len(t, enabled, 1)
	assert.Equal(t, "a", enabled[0].MemoryType)
	all := r.ListAll(true)
	assert.Len(t, all, 2)
}

func TestRegistry_ListNamesSorted(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "c", Enabled: true})
	_ = r.Register(MemoryTypeSchema{MemoryType: "a", Enabled: true})
	_ = r.Register(MemoryTypeSchema{MemoryType: "b", Enabled: true})
	names := r.ListNames(false)
	assert.Equal(t, []string{"a", "b", "c"}, names)
}

func TestRegistry_ListSearchURIs(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "a", Directory: "viking://user/{{ user_space }}/memories", Enabled: true})
	_ = r.Register(MemoryTypeSchema{MemoryType: "b", Directory: "", Enabled: true})
	_ = r.Register(MemoryTypeSchema{MemoryType: "c", Directory: "viking://user/{{ user_space }}/skills", Enabled: false})
	uris := r.ListSearchURIs("alice")
	assert.Equal(t, []string{"viking://user/alice/memories"}, uris)
	// Default user_space.
	uris = r.ListSearchURIs("")
	assert.Contains(t, uris[0], "default")
}

func TestRegistry_LoadFromYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yamlContent := `
memory_type: profile
description: User profile
filename_template: profile.md
directory: viking://user/{{ user_space }}/memories
enabled: true
operation_mode: upsert
stage: user
peer_enabled: true
fields:
  - name: name
    type: string
    description: User name
    merge_op: replace
  - name: age
    type: int64
    description: User age
    merge_op: sum
  - name: tags
    type: string
    description: Tags
    merge_op: patch
    init_value: ""
`
	path := filepath.Join(dir, "profile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0644))
	r := NewMemoryTypeRegistry()
	require.NoError(t, r.LoadFromYAML(path, false))
	mt, ok := r.Get("profile")
	require.True(t, ok)
	assert.Equal(t, "profile", mt.MemoryType)
	assert.Equal(t, "User profile", mt.Description)
	assert.True(t, mt.Enabled)
	assert.True(t, mt.PeerEnabled)
	require.Len(t, mt.Fields, 3)
	assert.Equal(t, "name", mt.Fields[0].Name)
	assert.Equal(t, FieldTypeString, mt.Fields[0].FieldType)
	assert.Equal(t, MergeOpReplace, mt.Fields[0].MergeOp)
	assert.Equal(t, "age", mt.Fields[1].Name)
	assert.Equal(t, FieldTypeInt64, mt.Fields[1].FieldType)
	assert.Equal(t, MergeOpSum, mt.Fields[1].MergeOp)
	assert.Equal(t, "tags", mt.Fields[2].Name)
	assert.Equal(t, MergeOpPatch, mt.Fields[2].MergeOp)
}

func TestRegistry_LoadFromYAML_DuplicateWithoutReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "memory_type: x\nenabled: true\n"
	p := filepath.Join(dir, "x.yaml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0644))
	r := NewMemoryTypeRegistry()
	require.NoError(t, r.LoadFromYAML(p, false))
	err := r.LoadFromYAML(p, false)
	assert.Error(t, err)
	// With replace, succeeds.
	require.NoError(t, r.LoadFromYAML(p, true))
}

func TestRegistry_LoadFromDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("memory_type: a\nenabled: true\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.yml"), []byte("memory_type: b\nenabled: false\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.txt"), []byte("ignored"), 0644))
	r := NewMemoryTypeRegistry()
	count := r.LoadFromDirectory(dir, false)
	assert.Equal(t, 2, count)
	_, ok := r.Get("a")
	assert.True(t, ok)
	_, ok = r.Get("b")
	assert.True(t, ok)
}

func TestRegistry_LoadFromDirectory_MissingDirReturnsZero(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	count := r.LoadFromDirectory("/nonexistent/path", false)
	assert.Equal(t, 0, count)
}

func TestRegistry_ParseMemoryType_EnableAlias(t *testing.T) {
	t.Parallel()
	mt, err := ParseMemoryType(map[string]any{
		"name":   "skills",
		"enable": false,
	})
	require.NoError(t, err)
	assert.Equal(t, "skills", mt.MemoryType)
	assert.False(t, mt.Enabled)
}

func TestRegistry_ParseMemoryType_Defaults(t *testing.T) {
	t.Parallel()
	mt, err := ParseMemoryType(map[string]any{"memory_type": "x"})
	require.NoError(t, err)
	assert.Equal(t, "x", mt.MemoryType)
	assert.True(t, mt.Enabled)
	assert.True(t, mt.PeerEnabled)
	assert.Equal(t, "upsert", mt.OperationMode)
	assert.Equal(t, "user", mt.Stage)
}

func TestRegistry_ParseMemoryType_RequiresName(t *testing.T) {
	t.Parallel()
	_, err := ParseMemoryType(map[string]any{"description": "no name"})
	assert.Error(t, err)
}

// ----- schema tests -----

func TestToPascalCase(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"session_skills", "SessionSkills"},
		{"kebab-case-name", "KebabCaseName"},
		{"alreadyPascal", "AlreadyPascal"},
		{"with spaces", "WithSpaces"},
		{"", ""},
		{"---", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ToPascalCase(c.in))
	}
}

func TestSchemaModelGenerator_CreateFlatDataModel(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{
		MemoryType: "profile",
		Enabled:    true,
		Fields: []MemoryField{
			{Name: "name", FieldType: FieldTypeString, MergeOp: MergeOpImmutable, Description: "User name"},
			{Name: "tags", FieldType: FieldTypeString, MergeOp: MergeOpPatch, Description: "Tags list"},
		},
	})
	g := NewSchemaModelGenerator(r, nil)
	mt, _ := r.Get("profile")
	schema := g.CreateFlatDataModel(mt, nil)
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "page_id")
	assert.Contains(t, props, "name")
	assert.Contains(t, props, "tags")
	// page_id and name (immutable) are required; tags is optional.
	reqs, _ := schema["required"].([]string)
	assert.Contains(t, reqs, "page_id")
	assert.Contains(t, reqs, "name")
	assert.NotContains(t, reqs, "tags")
}

func TestSchemaModelGenerator_CreateFlatDataModel_PeerScope(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{
		MemoryType:   "profile",
		Enabled:      true,
		PeerEnabled:  true,
		Fields:       []MemoryField{{Name: "name", FieldType: FieldTypeString, MergeOp: MergeOpImmutable}},
	})
	g := NewSchemaModelGenerator(r, nil)
	mt, _ := r.Get("profile")
	rs := &RoleScope{PeerIDs: []string{"alice", "bob"}}
	schema := g.CreateFlatDataModel(mt, rs)
	props, _ := schema["properties"].(map[string]any)
	assert.Contains(t, props, "peer_id")
	// Cached on the peer key; second call returns the same schema.
	schema2 := g.CreateFlatDataModel(mt, rs)
	assert.Equal(t, schema, schema2)
}

func TestSchemaModelGenerator_CreateFlatDataModel_RangesSkipsPeerID(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{
		MemoryType:   "events",
		Enabled:      true,
		PeerEnabled:  true,
		Fields:       []MemoryField{{Name: "ranges", FieldType: FieldTypeString, MergeOp: MergeOpPatch}},
	})
	g := NewSchemaModelGenerator(r, nil)
	mt, _ := r.Get("events")
	rs := &RoleScope{PeerIDs: []string{"alice"}}
	schema := g.CreateFlatDataModel(mt, rs)
	props, _ := schema["properties"].(map[string]any)
	assert.NotContains(t, props, "peer_id", "events schema has ranges, peer_id should be skipped")
}

func TestSchemaModelGenerator_GenerateAllModels(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "a", Enabled: true})
	_ = r.Register(MemoryTypeSchema{MemoryType: "b", Enabled: true})
	g := NewSchemaModelGenerator(r, nil)
	all := g.GenerateAllModels()
	assert.Len(t, all, 2)
	assert.Contains(t, all, "a")
	assert.Contains(t, all, "b")
}

func TestSchemaModelGenerator_CreateDiscriminatedUnionModel(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "a", Enabled: true})
	_ = r.Register(MemoryTypeSchema{MemoryType: "b", Enabled: true})
	g := NewSchemaModelGenerator(r, nil)
	schema := g.CreateDiscriminatedUnionModel()
	oneOf, ok := schema["oneOf"].([]map[string]any)
	require.True(t, ok)
	assert.Len(t, oneOf, 2)
	// Cached: second call returns the same map.
	assert.Equal(t, schema, g.CreateDiscriminatedUnionModel())
}

func TestSchemaModelGenerator_CreateDiscriminatedUnionModel_Empty(t *testing.T) {
	t.Parallel()
	g := NewSchemaModelGenerator([]MemoryTypeSchema{}, nil)
	schema := g.CreateDiscriminatedUnionModel()
	assert.Equal(t, "GenericMemoryData", schema["title"])
}

func TestSchemaModelGenerator_CreateStructuredOperationsModel(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "profile", Enabled: true, OperationMode: "upsert"})
	_ = r.Register(MemoryTypeSchema{MemoryType: "trajectories", Enabled: true, OperationMode: "add_only"})
	g := NewSchemaModelGenerator(r, nil)
	schema := g.CreateStructuredOperationsModel(nil)
	props, _ := schema["properties"].(map[string]any)
	assert.Contains(t, props, "profile")
	assert.Contains(t, props, "trajectories")
	assert.Contains(t, props, "delete_ids", "delete_ids should appear when at least one schema is deletable")
	// links is not enabled by default.
	assert.NotContains(t, props, "links")
}

func TestSchemaModelGenerator_CreateStructuredOperationsModel_LinkEnabled(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "profile", Enabled: true, OperationMode: "upsert"})
	g := NewSchemaModelGenerator(r, nil)
	g.SetLinkEnabled(true)
	schema := g.CreateStructuredOperationsModel(nil)
	props, _ := schema["properties"].(map[string]any)
	assert.Contains(t, props, "links")
}

func TestSchemaModelGenerator_CreateStructuredOperationsModel_NoDeleteWhenAllAddOnly(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "trajectories", Enabled: true, OperationMode: "add_only"})
	g := NewSchemaModelGenerator(r, nil)
	schema := g.CreateStructuredOperationsModel(nil)
	props, _ := schema["properties"].(map[string]any)
	assert.NotContains(t, props, "delete_ids")
}

func TestSchemaModelGenerator_GetLLMJSONSchema(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "profile", Enabled: true})
	g := NewSchemaModelGenerator(r, nil)
	schema := g.GetLLMJSONSchema(nil)
	assert.Equal(t, "StructuredMemoryOperations", schema["title"])
}

func TestSchemaModelGenerator_GetMemoryDataJSONSchema(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{MemoryType: "profile", Enabled: true})
	g := NewSchemaModelGenerator(r, nil)
	schema := g.GetMemoryDataJSONSchema()
	assert.Equal(t, "MemoryData", schema["title"])
}

func TestSchemaPromptGenerator_GenerateTypeDescriptions(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{
		MemoryType:       "profile",
		Description:      "User profile",
		Directory:        "viking://user/{{ user_space }}/memories",
		FilenameTemplate: "profile.md",
		Enabled:          true,
		Fields: []MemoryField{
			{Name: "name", FieldType: FieldTypeString, Description: "User name"},
		},
	})
	g := NewSchemaPromptGenerator(r, nil)
	out := g.GenerateTypeDescriptions()
	assert.Contains(t, out, "## Available Memory Types")
	assert.Contains(t, out, "### profile")
	assert.Contains(t, out, "User profile")
	assert.Contains(t, out, "viking://user/{{ user_space }}/memories/profile.md")
	assert.Contains(t, out, "`name` (string): User name")
}

func TestSchemaPromptGenerator_GenerateFieldDescriptions(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{
		MemoryType: "profile",
		Enabled:    true,
		Fields: []MemoryField{
			{Name: "name", FieldType: FieldTypeString, Description: "User name"},
			{Name: "age", FieldType: FieldTypeInt64, Description: "User age"},
		},
	})
	g := NewSchemaPromptGenerator(r, nil)
	out := g.GenerateFieldDescriptions("profile")
	assert.Contains(t, out, "### profile Fields")
	assert.Contains(t, out, "`name`: User name")
	assert.Contains(t, out, "`age`: User age")
	// Missing type returns empty.
	assert.Equal(t, "", g.GenerateFieldDescriptions("missing"))
}

func TestSchemaPromptGenerator_GetFullPromptContext(t *testing.T) {
	t.Parallel()
	r := NewMemoryTypeRegistry()
	_ = r.Register(MemoryTypeSchema{
		MemoryType:  "profile",
		Description: "User profile",
		Enabled:     true,
		Fields: []MemoryField{
			{Name: "name", FieldType: FieldTypeString, Description: "User name", MergeOp: MergeOpPatch},
		},
	})
	g := NewSchemaPromptGenerator(r, nil)
	ctx := g.GetFullPromptContext()
	assert.Contains(t, ctx, "type_descriptions")
	assert.Contains(t, ctx, "memory_types")
	mts, _ := ctx["memory_types"].([]map[string]any)
	require.Len(t, mts, 1)
	assert.Equal(t, "profile", mts[0]["memory_type"])
}

// ----- tools tests -----

type stubVikingFS struct {
	readContent string
	readErr     error
	searchRes   map[string]any
	searchErr   error
	lsEntries   []LsEntry
	lsErr       error
}

func (s *stubVikingFS) ReadFile(ctx context.Context, uri string, requestCtx any) (string, error) {
	return s.readContent, s.readErr
}

func (s *stubVikingFS) Search(ctx context.Context, query, targetURI string, limit int, requestCtx any) (SearchResult, error) {
	return stubSearchResult{s.searchRes}, s.searchErr
}

func (s *stubVikingFS) Ls(ctx context.Context, uri, output string, absLimit int, showAllHidden bool, nodeLimit int, requestCtx any) ([]LsEntry, error) {
	return s.lsEntries, s.lsErr
}

type stubSearchResult struct{ d map[string]any }

func (s stubSearchResult) ToDict() map[string]any { return s.d }

func TestMemoryReadTool_Schema(t *testing.T) {
	t.Parallel()
	tool := NewMemoryReadTool()
	assert.Equal(t, "read", tool.Name())
	schema := tool.ToSchema()
	fn, _ := schema["function"].(map[string]any)
	assert.Equal(t, "read", fn["name"])
}

func TestMemoryReadTool_Execute(t *testing.T) {
	t.Parallel()
	content := "---\nversion: 1\nmemory_type: profile\n---\n# Profile\nHello world\n"
	fs := &stubVikingFS{readContent: content}
	tc := &ToolContext{
		VikingFS:         fs,
		ReadFileContents: make(map[string]MemoryFile),
		PageIDMap:        NewPageIdMap(),
	}
	tool := NewMemoryReadTool()
	result, err := tool.Execute(nil, tc, map[string]any{"uri": "viking://user/u/memories/profile.md"})
	require.NoError(t, err)
	m, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, m, "content")
	assert.Contains(t, m, "page_id")
	assert.Contains(t, m["content"], "Hello world")
	// ReadFileContents cache is populated.
	_, ok = tc.ReadFileContents["viking://user/u/memories/profile.md"]
	assert.True(t, ok)
}

func TestMemoryReadTool_Error(t *testing.T) {
	t.Parallel()
	fs := &stubVikingFS{readErr: assert.AnError}
	tc := &ToolContext{VikingFS: fs}
	tool := NewMemoryReadTool()
	result, _ := tool.Execute(nil, tc, map[string]any{"uri": "missing.md"})
	m, _ := result.(map[string]any)
	assert.Contains(t, m, "error")
}

func TestMemoryReadTool_NilContext(t *testing.T) {
	t.Parallel()
	tool := NewMemoryReadTool()
	result, _ := tool.Execute(nil, nil, map[string]any{"uri": "x"})
	m, _ := result.(map[string]any)
	assert.Contains(t, m, "error")
}

func TestMemoryReadTool_OffsetBeyondEnd(t *testing.T) {
	t.Parallel()
	content := "line1\nline2\n"
	fs := &stubVikingFS{readContent: content}
	tc := &ToolContext{VikingFS: fs}
	tool := NewMemoryReadTool()
	result, _ := tool.Execute(nil, tc, map[string]any{"uri": "x", "offset": 100})
	m, _ := result.(map[string]any)
	assert.Contains(t, m["content"], "shorter than the provided offset")
}

func TestMemoryReadTool_EmptyContent(t *testing.T) {
	t.Parallel()
	fs := &stubVikingFS{readContent: ""}
	tc := &ToolContext{VikingFS: fs}
	tool := NewMemoryReadTool()
	result, _ := tool.Execute(nil, tc, map[string]any{"uri": "x"})
	m, _ := result.(map[string]any)
	assert.Contains(t, m["content"], "contents are empty")
}

func TestMemorySearchTool_Execute(t *testing.T) {
	t.Parallel()
	fs := &stubVikingFS{searchRes: map[string]any{
		"memories": []any{
			map[string]any{"uri": "viking://user/u/memories/a.md", "score": 0.9},
			map[string]any{"uri": "viking://user/u/memories/b.abstract.md", "score": 0.8},
		},
	}}
	tc := &ToolContext{VikingFS: fs, DefaultSearchURIs: "viking://user/u/memories"}
	tool := NewMemorySearchTool()
	result, _ := tool.Execute(nil, tc, map[string]any{"query": "hello"})
	out, ok := result.([]any)
	require.True(t, ok)
	require.Len(t, out, 1, "abstract should be filtered out")
	m, _ := out[0].(map[string]any)
	assert.Equal(t, "viking://user/u/memories/a.md", m["uri"])
}

func TestMemorySearchTool_Error(t *testing.T) {
	t.Parallel()
	fs := &stubVikingFS{searchErr: assert.AnError}
	tc := &ToolContext{VikingFS: fs}
	tool := NewMemorySearchTool()
	result, _ := tool.Execute(nil, tc, map[string]any{"query": "x"})
	m, _ := result.(map[string]any)
	assert.Contains(t, m, "error")
}

func TestMemoryLsTool_Execute(t *testing.T) {
	t.Parallel()
	fs := &stubVikingFS{lsEntries: []LsEntry{
		{"name": "a.md", "size": int(1024), "isDir": false},
		{"name": "b.md", "size": int(2048), "isDir": false},
		{"name": "subdir", "size": int(0), "isDir": true},
	}}
	tc := &ToolContext{VikingFS: fs}
	tool := NewMemoryLsTool()
	result, _ := tool.Execute(nil, tc, map[string]any{"uri": "viking://user/u/memories"})
	s, _ := result.(string)
	assert.Contains(t, s, "a.md 1.0K")
	assert.Contains(t, s, "b.md 2.0K")
	assert.NotContains(t, s, "subdir")
}

func TestMemoryLsTool_Empty(t *testing.T) {
	t.Parallel()
	fs := &stubVikingFS{}
	tc := &ToolContext{VikingFS: fs}
	tool := NewMemoryLsTool()
	result, _ := tool.Execute(nil, tc, map[string]any{"uri": "x"})
	s, _ := result.(string)
	assert.Contains(t, s, "Directory is empty")
}

func TestMemoryLsTool_NameFallbackToURI(t *testing.T) {
	t.Parallel()
	fs := &stubVikingFS{lsEntries: []LsEntry{
		{"uri": "viking://user/u/memories/c.md", "size": int(0), "isDir": false},
	}}
	tc := &ToolContext{VikingFS: fs}
	tool := NewMemoryLsTool()
	result, _ := tool.Execute(nil, tc, map[string]any{"uri": "x"})
	s, _ := result.(string)
	assert.Contains(t, s, "c.md")
}

func TestMemoryToolsRegistry(t *testing.T) {
	t.Parallel()
	r := NewMemoryToolsRegistry()
	r.Register(NewMemoryReadTool())
	r.Register(NewMemorySearchTool())
	r.Register(NewMemoryLsTool())
	assert.NotNil(t, r.Get("read"))
	assert.Nil(t, r.Get("missing"))
	schemas := r.GetToolSchemas()
	// Only "read" is in LLM_TOOLS.
	assert.Len(t, schemas, 1)
}

func TestDefaultMemoryTools(t *testing.T) {
	t.Parallel()
	assert.NotNil(t, DefaultMemoryTools)
	assert.NotNil(t, GetTool("read"))
	assert.NotNil(t, GetTool("search"))
	assert.NotNil(t, GetTool("ls"))
	assert.Nil(t, GetTool("missing"))
	schemas := GetToolSchemas()
	assert.Len(t, schemas, 1)
}

func TestLLMHiddenMemoryFields(t *testing.T) {
	t.Parallel()
	assert.Contains(t, LLMHiddenMemoryFields, "source_extraction_id")
	assert.Contains(t, LLMHiddenMemoryFields, "source_extraction_ids")
	assert.Contains(t, LLMHiddenMemoryFields, "last_update_trace_id")
}
