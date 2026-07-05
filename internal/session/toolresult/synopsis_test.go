// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package toolresult

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateSynopsis_Empty(t *testing.T) {
	t.Parallel()
	syn := GenerateSynopsis("", 0, "", "")
	assert.Equal(t, KindUnknown, syn.Kind)
	assert.Equal(t, "Empty output", syn.Title)
	assert.Equal(t, []string{"Output is empty."}, syn.Summary)
}

func TestGenerateSynopsis_JSON_Object(t *testing.T) {
	t.Parallel()
	content := `{"name": "alice", "age": 30, "tags": ["a", "b"]}`
	syn := GenerateSynopsis(content, 0, "tool", "application/json")
	assert.Equal(t, KindJSON, syn.Kind)
	assert.Equal(t, "JSON output", syn.Title)
	assert.Contains(t, syn.Summary[0], "JSON object.")
	// top-level keys line lists the object keys (order is map-iteration random).
	joinedSummary := strings.Join(syn.Summary, " ")
	assert.True(t,
		strings.Contains(joinedSummary, "name") && strings.Contains(joinedSummary, "age") && strings.Contains(joinedSummary, "tags"),
		"summary should list top-level keys: %s", joinedSummary)
	// Structure should mention shape and per-key types.
	joined := strings.Join(syn.Structure, "\n")
	assert.Contains(t, joined, "shape:")
	assert.Contains(t, joined, "name: string")
	assert.Contains(t, joined, "tags: array length: 2")
	// Notable items should include the scalar value example for "name".
	joinedNotable := strings.Join(syn.NotableItems, "\n")
	assert.Contains(t, joinedNotable, "alice")
}

func TestGenerateSynopsis_JSON_Array(t *testing.T) {
	t.Parallel()
	content := `[{"id": 1}, {"id": 2}, {"id": 3}]`
	syn := GenerateSynopsis(content, 0, "", "")
	assert.Equal(t, KindJSON, syn.Kind)
	assert.Contains(t, syn.Summary[1], "array length: 3")
	joined := strings.Join(syn.Structure, "\n")
	assert.Contains(t, joined, "first item type: object")
}

func TestGenerateSynopsis_JSON_TrailingChars(t *testing.T) {
	t.Parallel()
	content := `{"a": 1} extra junk`
	syn := GenerateSynopsis(content, 0, "", "")
	assert.Equal(t, KindJSON, syn.Kind)
	assert.Contains(t, strings.Join(syn.NotableItems, " "), "trailing_chars_after_first_json_value")
}

func TestGenerateSynopsis_JSON_MalformedWithMime(t *testing.T) {
	t.Parallel()
	content := `{not valid json`
	syn := GenerateSynopsis(content, 0, "", "application/json")
	assert.Equal(t, KindUnknown, syn.Kind)
	assert.Equal(t, "Unparsed JSON-like output", syn.Title)
}

func TestGenerateSynopsis_XML(t *testing.T) {
	t.Parallel()
	content := `<root><child/><child/><other/></root>`
	syn := GenerateSynopsis(content, 0, "", "")
	assert.Equal(t, KindXML, syn.Kind)
	assert.Equal(t, "XML output", syn.Title)
	joined := strings.Join(syn.Structure, "\n")
	assert.Contains(t, joined, "root: root")
	assert.Contains(t, joined, "child: 2")
	assert.Contains(t, joined, "other: 1")
}

func TestGenerateSynopsis_CSV(t *testing.T) {
	t.Parallel()
	content := "name,age\nalice,30\nbob,25\n"
	syn := GenerateSynopsis(content, 0, "", "")
	assert.Equal(t, KindCSV, syn.Kind)
	assert.Contains(t, syn.Summary[0], "rows: 2 and columns: 2")
	joined := strings.Join(syn.Structure, "\n")
	assert.Contains(t, joined, "name, age")
	assert.Contains(t, joined, "rows: 2")
}

func TestGenerateSynopsis_TSV(t *testing.T) {
	t.Parallel()
	content := "name\tage\nalice\t30\nbob\t25\n"
	syn := GenerateSynopsis(content, 0, "", "")
	assert.Equal(t, KindTSV, syn.Kind)
}

func TestGenerateSynopsis_YAML(t *testing.T) {
	t.Parallel()
	content := "name: alice\nage: 30\ntags:\n  - a\n  - b\n"
	syn := GenerateSynopsis(content, 0, "", "")
	assert.Equal(t, KindYAML, syn.Kind)
	joined := strings.Join(syn.Structure, "\n")
	assert.Contains(t, joined, "name:")
	assert.Contains(t, joined, "age:")
}

func TestGenerateSynopsis_Code(t *testing.T) {
	t.Parallel()
	content := "import os\nfrom pathlib import Path\n\ndef hello():\n    pass\n"
	syn := GenerateSynopsis(content, 0, "", "")
	assert.Equal(t, KindCode, syn.Kind)
	joined := strings.Join(syn.Structure, "\n")
	assert.Contains(t, joined, "line_count:")
	assert.Contains(t, joined, "imports:")
	assert.Contains(t, joined, "os")
	// def hello should show up in notable_items.
	assert.Contains(t, strings.Join(syn.NotableItems, " "), "def hello")
}

func TestGenerateSynopsis_Text(t *testing.T) {
	t.Parallel()
	content := "# Title\n\nSome paragraph here.\n\n## Section\n\nMore text."
	syn := GenerateSynopsis(content, 0, "", "")
	assert.Equal(t, KindText, syn.Kind)
	joined := strings.Join(syn.Summary, "\n")
	assert.Contains(t, joined, "Characters:")
	assert.Contains(t, joined, "Words:")
	assert.Contains(t, joined, "Lines:")
	assert.Contains(t, joined, "# Title")
}

func TestGenerateSynopsis_Text_AllCapsHeaders(t *testing.T) {
	t.Parallel()
	content := "INTRODUCTION\n\nbody\n\nMETHODS\n\nmore body\n"
	syn := GenerateSynopsis(content, 0, "", "")
	assert.Equal(t, KindText, syn.Kind)
	joined := strings.Join(syn.Summary, "\n")
	assert.Contains(t, joined, "INTRODUCTION")
	assert.Contains(t, joined, "METHODS")
}

func TestRenderToolResultStub_Full(t *testing.T) {
	t.Parallel()
	syn := ToolResultSynopsis{
		Kind:      KindJSON,
		Title:     "JSON output",
		Summary:   []string{"JSON object."},
		Structure: []string{"shape: object"},
		NotableItems: []string{"name: alice"},
	}
	out := RenderToolResultStub(syn, "ref-123", "search", "abcdef0123456789", "too big", 5000, 200)
	assert.Contains(t, out, "[OpenViking tool result externalized]")
	assert.Contains(t, out, "tool_name: search")
	assert.Contains(t, out, "kind: json")
	assert.Contains(t, out, "original_chars: 5000")
	assert.Contains(t, out, "preview_chars: 200")
	assert.Contains(t, out, "ref: ref-123")
	assert.Contains(t, out, "sha256: abcdef0123456789")
	assert.Contains(t, out, "reason: too big")
	assert.Contains(t, out, "Synopsis:")
	assert.Contains(t, out, "- JSON object.")
	assert.Contains(t, out, "Structure:")
	assert.Contains(t, out, "- shape: object")
	assert.Contains(t, out, "Notable items:")
	assert.Contains(t, out, "- name: alice")
	assert.Contains(t, out, "Explore:")
	assert.Contains(t, out, "openviking_tool_result_search")
	assert.Contains(t, out, "openviking_tool_result_read")
	assert.Contains(t, out, "openviking_tool_result_list")
}

func TestRenderToolResultStub_NoRefNoExtras(t *testing.T) {
	t.Parallel()
	syn := ToolResultSynopsis{Kind: KindText, Title: "Text output", Summary: []string{"Text."}}
	out := RenderToolResultStub(syn, "", "", "", "", 100, 50)
	assert.NotContains(t, out, "ref:")
	assert.NotContains(t, out, "sha256:")
	assert.NotContains(t, out, "reason:")
	assert.NotContains(t, out, "Explore:")
}

func TestToolResultSynopsis_ToMap_FromMap_RoundTrip(t *testing.T) {
	t.Parallel()
	original := ToolResultSynopsis{
		Kind:         KindCSV,
		Title:        "title",
		Summary:      []string{"a", "b"},
		Structure:    []string{"c"},
		NotableItems: []string{"d"},
		Sample:       "sample text",
	}
	m := original.ToMap()
	assert.Equal(t, "csv", m["kind"])
	// ToMap converts string slices to []any so JSON round-trip stays stable.
	assert.Equal(t, []any{"a", "b"}, m["summary"])
	assert.Equal(t, []any{"c"}, m["structure"])
	assert.Equal(t, []any{"d"}, m["notable_items"])
	assert.Equal(t, "sample text", m["sample"])

	roundTrip := SynopsisFromMap(m)
	assert.Equal(t, original, roundTrip)
}

func TestSynopsisFromMap_Defaults(t *testing.T) {
	t.Parallel()
	syn := SynopsisFromMap(map[string]any{})
	assert.Equal(t, KindUnknown, syn.Kind)
	assert.Equal(t, "", syn.Title)
	// After the fix, empty slices round-trip as []string{}, not nil.
	assert.Equal(t, []string{}, syn.Summary)
	assert.Equal(t, []string{}, syn.Structure)
	assert.Equal(t, []string{}, syn.NotableItems)
}

func TestBuildToolResultID_WithToolID(t *testing.T) {
	t.Parallel()
	id := BuildToolResultID("search_tool", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	assert.Equal(t, "tr_search_tool_0123456789abcdef", id)
}

func TestBuildToolResultID_NoToolID(t *testing.T) {
	t.Parallel()
	id := BuildToolResultID("", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	// Python: sha256[:24] = 24 hex chars.
	assert.Equal(t, "tr_0123456789abcdef01234567", id)
}

func TestBuildToolResultID_SanitizesBadChars(t *testing.T) {
	t.Parallel()
	id := BuildToolResultID("bad/id with spaces", "abcdef0123456789")
	assert.Contains(t, id, "tr_")
	assert.NotContains(t, id, "/")
	assert.NotContains(t, id, " ")
}

func TestValidateToolResultID(t *testing.T) {
	t.Parallel()
	assert.True(t, ValidateToolResultID("tr_search_abcdef0123456789"))
	assert.True(t, ValidateToolResultID("tr_abcdef"))
	assert.False(t, ValidateToolResultID(""))
	assert.False(t, ValidateToolResultID("with/slash"))
	assert.False(t, ValidateToolResultID("with space"))
	assert.False(t, ValidateToolResultID("with#hash"))
}

func TestSafeID(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "alice", SafeID("alice", "tool"))
	assert.Equal(t, "alice_bob", SafeID("alice bob", "tool"))
	assert.Equal(t, "alice_bob", SafeID("alice/bob", "tool"))
	assert.Equal(t, "alice", SafeID("...alice---", "tool"))
	assert.Equal(t, "tool", SafeID("", "tool"))
	assert.Equal(t, "tool", SafeID("   ", "tool"))
	// Truncation at 96.
	long := strings.Repeat("a", 200)
	got := SafeID(long, "tool")
	assert.LessOrEqual(t, len(got), 96)
}

func TestLooksBinary_ControlChars(t *testing.T) {
	t.Parallel()
	assert.False(t, looksBinary("hello world"))
	assert.True(t, looksBinary("hello\x00world"))
	// Lots of control chars in first 1000 → binary.
	bin := strings.Repeat("\x01\x02", 600)
	assert.True(t, looksBinary(bin))
}

func TestHeadTailSample_ShortContent(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", headTailSample("abc", 0))
	assert.Equal(t, "abc", headTailSample("abc", 100))
}

func TestHeadTailSample_LongContent(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("a", 200)
	out := headTailSample(content, 100)
	assert.Contains(t, out, "BEGIN SAMPLE HEAD")
	assert.Contains(t, out, "END SAMPLE HEAD")
	assert.Contains(t, out, "BEGIN SAMPLE TAIL")
	assert.Contains(t, out, "END SAMPLE TAIL")
}

func TestClip(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", clip("hello", 0))
	// len("hello")=5 <= limit=5 → returned unchanged.
	assert.Equal(t, "hello", clip("hello", 5))
	// len("hello world")=11 > limit=6 → cut at 3 + "...".
	assert.Equal(t, "hel...", clip("hello world", 6))
	// limit < 3 → cutAt clamped to 0, so just "...".
	assert.Equal(t, "...", clip("hello world", 2))
}

func TestSHA256Text(t *testing.T) {
	t.Parallel()
	// "abc" → sha256 hex
	assert.Equal(t,
		"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		SHA256Text("abc"))
}
