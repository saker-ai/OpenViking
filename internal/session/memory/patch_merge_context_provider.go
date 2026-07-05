// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// patchMergeSystemHiddenFields is the set of metadata fields stripped
// from the patch-merge diff output. It mirrors
// patch_merge_context_provider._SYSTEM_HIDDEN_FIELDS.
var patchMergeSystemHiddenFields = map[string]struct{}{
	"source_extraction_id":  {},
	"source_extraction_ids": {},
	"last_update_trace_id":  {},
}

// PatchMergeMaxExtraCandidateFiles caps the number of extra candidate
// files discovered via semantic search during prefetch.
const PatchMergeMaxExtraCandidateFiles = 10

// PatchMergePatchMetadataKeys is the set of patch-metadata keys kept in
// the rendered diff (everything else is stripped).
var PatchMergePatchMetadataKeys = []string{"confidence"}

// PatchMergePatch is one before/after memory-file patch rendered as
// field-level line diffs. It mirrors
// openviking.session.memory.patch_merge_context_provider.PatchMergePatch.
type PatchMergePatch struct {
	BeforeFile *MemoryFile
	AfterFile  MemoryFile
	Metadata   map[string]any
}

// TargetURI returns the URI the patch applies to. It mirrors
// PatchMergePatch.target_uri.
func (p PatchMergePatch) TargetURI() string {
	if p.AfterFile.URI != "" {
		return p.AfterFile.URI
	}
	if p.BeforeFile != nil {
		return p.BeforeFile.URI
	}
	return ""
}

// MemoryType returns the memory_type of the patch. It mirrors
// PatchMergePatch.memory_type.
func (p PatchMergePatch) MemoryType() string {
	if p.AfterFile.MemoryType != "" {
		return p.AfterFile.MemoryType
	}
	if p.BeforeFile != nil && p.BeforeFile.MemoryType != "" {
		return p.BeforeFile.MemoryType
	}
	if v, ok := p.AfterFile.ExtraFields["memory_type"].(string); ok && v != "" {
		return v
	}
	if p.BeforeFile != nil {
		if v, ok := p.BeforeFile.ExtraFields["memory_type"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// TargetName returns the human-readable name for the patch target. It
// mirrors PatchMergePatch.target_name.
func (p PatchMergePatch) TargetName() string {
	fields := p.AfterFile.ExtraFields
	if fields == nil {
		fields = map[string]any{}
	}
	memoryType := p.MemoryType()
	typeSpecificKey := strings.TrimSuffix(memoryType, "s") + "_name"
	for _, key := range []string{typeSpecificKey, "experience_name", "name"} {
		if v, ok := fields[key].(string); ok && v != "" {
			return v
		}
	}
	uri := p.TargetURI()
	if uri == "" {
		return "unknown"
	}
	if strings.HasSuffix(uri, "/SKILL.md") {
		parts := strings.Split(strings.TrimSuffix(uri, "/SKILL.md"), "/")
		if len(parts) >= 1 {
			return parts[len(parts)-1]
		}
	}
	if idx := strings.LastIndex(uri, "/"); idx >= 0 {
		return strings.TrimSuffix(uri[idx+1:], ".md")
	}
	return strings.TrimSuffix(uri, ".md")
}

// PatchMergeContextProvider provides original memory files and
// structured field diffs to ExtractLoop. It mirrors
// openviking.session.memory.patch_merge_context_provider.PatchMergeContextProvider.
type PatchMergeContextProvider struct {
	*SessionExtractContextProvider

	MemoryType       string
	Patches          []PatchMergePatch
	RequiredFileURIs []string
}

// NewPatchMergeContextProvider constructs the provider. It mirrors
// PatchMergeContextProvider.__init__.
func NewPatchMergeContextProvider(
	memoryType string,
	patches []PatchMergePatch,
	requiredFileURIs []string,
	outputLanguage string,
) *PatchMergeContextProvider {
	p := &PatchMergeContextProvider{
		SessionExtractContextProvider: NewSessionExtractContextProvider(nil, ""),
		MemoryType:                    memoryType,
		Patches:                       append([]PatchMergePatch(nil), patches...),
		RequiredFileURIs:              append([]string(nil), requiredFileURIs...),
	}
	if outputLanguage == "" {
		outputLanguage = ResolveOutputLanguageFromText(patchLanguageText(patches), "en")
	}
	p.OutputLanguage = outputLanguage
	return p
}

// Instruction returns the system prompt. It mirrors
// PatchMergeContextProvider.instruction.
func (p *PatchMergeContextProvider) Instruction() string {
	return fmt.Sprintf(`You are a memory patch merge agent.

You are given original memory files and structured memory-file field diffs. Merge them by producing final memory operations that follow the provided JSON schema.

Do not call tools. Output JSON only.

All memory content must be written in %s.

Reconcile independent extraction patch proposals: merge duplicate/overlapping
memories into one canonical file patch, and keep distinct memories separate.
Normalize URI/path variants for directory/filename fields. Treat path segment
fields as stable schema identifiers, not free-form labels. Reuse existing
equivalent directories across singular/plural, synonym, or language/script
variants. For new segments, use singular snake_case for English and one concise
canonical term for Chinese; e.g. book not books, 书籍 not 书/图书. If a loser URI
is an existing file, put it in delete_ids; if it is only a new proposal, omit it.
`, p.OutputLanguage)
}

// GetTools returns no tools. It mirrors
// PatchMergeContextProvider.get_tools.
func (p *PatchMergeContextProvider) GetTools() []string {
	return nil
}

// GetMemorySchemas returns the schema for the patch's memory type. It
// mirrors PatchMergeContextProvider.get_memory_schemas.
func (p *PatchMergeContextProvider) GetMemorySchemas(requestCtx any) []MemoryTypeSchema {
	schema, ok := p.getRegistry().Get(p.MemoryType)
	if !ok || !schema.Enabled {
		return nil
	}
	return []MemoryTypeSchema{schema}
}

// Prefetch returns the messages to feed the LLM. Each required file is
// read into the provider's cache, then the field-diff patches are
// appended as one user message. It mirrors
// PatchMergeContextProvider.prefetch.
func (p *PatchMergeContextProvider) Prefetch(ctx context.Context) ([]map[string]any, error) {
	out := make([]map[string]any, 0, 8)
	callID := 0
	fileURIs := p.resolvePrefetchFileURIs(ctx)
	for _, uri := range fileURIs {
		result, _ := p.readFile(ctx, uri)
		if result == nil {
			continue
		}
		out = AddToolCallPairToMessages(out, callID, "read",
			map[string]any{"uri": uri}, result)
		callID++
	}
	out = append(out, map[string]any{
		"role":    "user",
		"content": RenderFieldDiffPatches(p.Patches),
	})
	return out, nil
}

// resolvePrefetchFileURIs returns the required URIs plus semantic-search
// candidates. It mirrors PatchMergeContextProvider._resolve_prefetch_file_uris.
func (p *PatchMergeContextProvider) resolvePrefetchFileURIs(ctx context.Context) []string {
	requiredURIs := dedupeURIs(p.RequiredFileURIs)
	maxExtra := PatchMergeMaxExtraCandidateFiles
	if maxExtra > 5 && maxExtra > len(requiredURIs) {
		// keep
	}
	if maxExtra < 5 {
		maxExtra = 5
	}
	candidateURIs, _ := p.searchCandidateFileURIs(ctx, maxExtra*2)
	extraURIs := make([]string, 0, maxExtra)
	requiredSet := make(map[string]struct{}, len(requiredURIs))
	for _, uri := range requiredURIs {
		requiredSet[uri] = struct{}{}
	}
	for _, uri := range candidateURIs {
		if uri == "" {
			continue
		}
		if _, ok := requiredSet[uri]; ok {
			continue
		}
		seen := false
		for _, e := range extraURIs {
			if e == uri {
				seen = true
				break
			}
		}
		if seen {
			continue
		}
		extraURIs = append(extraURIs, uri)
		if len(extraURIs) >= maxExtra {
			break
		}
	}
	return append(requiredURIs, extraURIs...)
}

// searchCandidateFileURIs runs a semantic search for candidate files.
// It mirrors PatchMergeContextProvider._search_candidate_file_uris.
func (p *PatchMergeContextProvider) searchCandidateFileURIs(ctx context.Context, limit int) ([]string, error) {
	schema, ok := p.getRegistry().Get(p.MemoryType)
	if !ok || schema.Directory == "" {
		return nil, nil
	}
	searchDirs := p.renderSearchDirectories(schema)
	if len(searchDirs) == 0 {
		return nil, nil
	}
	query := BuildPatchSearchQuery(p.Patches)
	if query == "" {
		return nil, nil
	}
	return p.searchFiles(ctx, query, searchDirs, limit)
}

// renderSearchDirectories returns the directory URIs to search within.
// It mirrors PatchMergeContextProvider._render_search_directories.
func (p *PatchMergeContextProvider) renderSearchDirectories(schema MemoryTypeSchema) []string {
	if p.isolationHandler != nil {
		dirs := p.isolationHandler.RenderSchemaDirectories(schema)
		seen := make(map[string]struct{}, len(dirs))
		out := make([]string, 0, len(dirs))
		for _, d := range dirs {
			if _, ok := seen[d]; ok {
				continue
			}
			seen[d] = struct{}{}
			out = append(out, d)
		}
		return out
	}
	userID := ""
	if p.ctx != nil {
		userID = p.ctx.UserID
	}
	if userID == "" {
		userID = InferUserSpaceFromURIs(p.RequiredFileURIs)
	}
	if userID == "" {
		uris := make([]string, len(p.Patches))
		for i, patch := range p.Patches {
			uris[i] = patch.TargetURI()
		}
		userID = InferUserSpaceFromURIs(uris)
	}
	if userID == "" {
		return nil
	}
	rendered, err := RenderTemplate(schema.Directory, map[string]any{"user_space": userID})
	if err != nil {
		return nil
	}
	return []string{rendered}
}

// RenderFieldDiffPatches renders the patch list as a single string. It
// mirrors _render_field_diff_patches.
func RenderFieldDiffPatches(patches []PatchMergePatch) string {
	if len(patches) == 0 {
		return "# Memory File Patches\n\nNo patches provided."
	}
	rendered := make([]string, 0, len(patches))
	for idx, patch := range patches {
		rendered = append(rendered, renderOneFieldDiffPatch(idx+1, patch))
	}
	return "# Memory File Patches\n\n" + strings.Join(rendered, "\n\n")
}

// renderOneFieldDiffPatch renders one patch. It mirrors
// _render_one_field_diff_patch.
func renderOneFieldDiffPatch(index int, patch PatchMergePatch) string {
	lines := []string{fmt.Sprintf("Patch %d", index)}
	if len(patch.Metadata) > 0 {
		compact := CompactPatchMetadata(patch.Metadata)
		if len(compact) > 0 {
			lines = append(lines, "  meta: "+compactValue(compact))
		}
	}
	diffs := FieldDiffs(patch.BeforeFile, patch.AfterFile)
	if len(diffs) == 0 {
		lines = append(lines, "  (no changes)")
		return strings.Join(lines, "\n")
	}
	for _, fd := range diffs {
		lines = append(lines, "  "+fd.Name+":")
		for _, line := range strings.Split(fd.Diff, "\n") {
			if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
				continue
			}
			lines = append(lines, "    "+line)
		}
	}
	return strings.Join(lines, "\n")
}

// FieldDiff is one (field_name, unified_diff) pair.
type FieldDiff struct {
	Name string
	Diff string
}

// FieldDiffs returns the unified diffs for each changed field. It
// mirrors _field_diffs.
func FieldDiffs(beforeFile *MemoryFile, afterFile MemoryFile) []FieldDiff {
	beforeFields := memoryFileFields(beforeFile)
	afterFields := memoryFileFields(&afterFile)
	allNames := make(map[string]struct{}, len(beforeFields)+len(afterFields))
	for k := range beforeFields {
		allNames[k] = struct{}{}
	}
	for k := range afterFields {
		allNames[k] = struct{}{}
	}
	names := make([]string, 0, len(allNames))
	for k := range allNames {
		names = append(names, k)
	}
	sort.Strings(names)
	diffs := make([]FieldDiff, 0, len(names))
	for _, name := range names {
		beforeValue := beforeFields[name]
		afterValue := afterFields[name]
		if equalValue(beforeValue, afterValue) {
			continue
		}
		diff := valueUnifiedDiff(name, beforeValue, afterValue)
		if strings.TrimSpace(diff) == "" {
			continue
		}
		diffs = append(diffs, FieldDiff{Name: name, Diff: diff})
	}
	return diffs
}

// memoryFileFields returns the field map for one MemoryFile. It mirrors
// _memory_file_fields.
func memoryFileFields(file *MemoryFile) map[string]any {
	out := make(map[string]any, 8)
	if file == nil {
		return out
	}
	for k, v := range file.ExtraFields {
		if _, hidden := patchMergeSystemHiddenFields[k]; hidden {
			continue
		}
		out[k] = v
	}
	if file.MemoryType != "" {
		out["memory_type"] = file.MemoryType
	}
	if file.Content != "" {
		out["content"] = file.Content
	}
	if len(file.Links) > 0 {
		out["links"] = file.Links
	}
	if len(file.Backlinks) > 0 {
		out["backlinks"] = file.Backlinks
	}
	return out
}

// valueUnifiedDiff returns a unified diff between before and after. It
// mirrors _value_unified_diff.
func valueUnifiedDiff(fieldName string, beforeValue, afterValue any) string {
	beforeLines := valueLines(beforeValue)
	afterLines := valueLines(afterValue)
	return unifiedDiff(beforeLines, afterLines, fieldName+".before", fieldName+".after")
}

// valueLines splits a value into lines for diffing. It mirrors
// _value_lines.
func valueLines(value any) []string {
	if value == nil {
		return nil
	}
	switch x := value.(type) {
	case string:
		return strings.Split(x, "\n")
	default:
		data, err := json.MarshalIndent(x, "", "  ")
		if err != nil {
			return nil
		}
		return strings.Split(string(data), "\n")
	}
}

// unifiedDiff produces a minimal unified diff between two line slices.
// It mirrors difflib.unified_diff with n=1.
func unifiedDiff(before, after []string, fromFile, toFile string) string {
	out := strings.Builder{}
	out.WriteString("--- " + fromFile + "\n")
	out.WriteString("+++ " + toFile + "\n")
	// Compute LCS-style diff: common prefix + suffix, middle is the change.
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix && before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	hunkStart := prefix
	hunkEndBefore := len(before) - suffix
	hunkEndAfter := len(after) - suffix
	out.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n",
		hunkStart+1, hunkEndBefore-hunkStart, hunkStart+1, hunkEndAfter-hunkStart))
	for i := hunkStart; i < hunkEndBefore; i++ {
		out.WriteString("-" + before[i] + "\n")
	}
	for i := hunkStart; i < hunkEndAfter; i++ {
		out.WriteString("+" + after[i] + "\n")
	}
	return out.String()
}

// compactValue returns a compact JSON encoding of value. It mirrors
// _compact_value.
func compactValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

// CompactPatchMetadata keeps only the decision-signal keys. It mirrors
// _compact_patch_metadata.
func CompactPatchMetadata(metadata map[string]any) map[string]any {
	cleaned := hideSystemFields(metadata)
	out := make(map[string]any, len(PatchMergePatchMetadataKeys))
	for _, key := range PatchMergePatchMetadataKeys {
		v, ok := cleaned[key]
		if !ok {
			continue
		}
		if !metadataValueIsUseful(v) {
			continue
		}
		out[key] = v
	}
	return out
}

// hideSystemFields returns a copy of value with system-hidden fields
// stripped. It mirrors _hide_system_fields.
func hideSystemFields(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for k, v := range value {
		if _, hidden := patchMergeSystemHiddenFields[k]; hidden {
			continue
		}
		out[k] = v
	}
	return out
}

// metadataValueIsUseful reports whether v is non-empty. It mirrors
// _metadata_value_is_useful.
func metadataValueIsUseful(v any) bool {
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// dedupeURIs returns uris with duplicates and empties removed. It
// mirrors _dedupe_uris.
func dedupeURIs(uris []string) []string {
	seen := make(map[string]struct{}, len(uris))
	out := make([]string, 0, len(uris))
	for _, uri := range uris {
		if uri == "" {
			continue
		}
		if _, ok := seen[uri]; ok {
			continue
		}
		seen[uri] = struct{}{}
		out = append(out, uri)
	}
	return out
}

// BuildPatchSearchQuery builds a semantic-search query from the patch
// list. It mirrors _build_patch_search_query.
func BuildPatchSearchQuery(patches []PatchMergePatch) string {
	parts := make([]string, 0, len(patches)*3)
	for _, patch := range patches {
		if name := patch.TargetName(); name != "" {
			parts = append(parts, name)
		}
		if uri := patch.TargetURI(); uri != "" {
			base := uri
			if idx := strings.LastIndex(uri, "/"); idx >= 0 {
				base = uri[idx+1:]
			}
			base = strings.TrimSuffix(base, ".md")
			if base != "" {
				parts = append(parts, base)
			}
		}
		if content := patch.AfterFile.Content; content != "" {
			parts = append(parts, truncateQueryText(content, 1200))
		}
	}
	return truncateQueryText(strings.Join(parts, "\n\n"), 5000)
}

// truncateQueryText collapses whitespace and truncates to maxChars. It
// mirrors _truncate_query_text.
func truncateQueryText(text string, maxChars int) string {
	normalized := strings.Join(strings.Fields(text), " ")
	if len(normalized) <= maxChars {
		return normalized
	}
	return normalized[:maxChars-3] + "..."
}

// InferUserSpaceFromURIs extracts the user_space segment from the first
// viking://user/<space>/ URI in the list. It mirrors
// _infer_user_space_from_uris.
func InferUserSpaceFromURIs(uris []string) string {
	for _, uri := range uris {
		if uri == "" {
			continue
		}
		prefix := "viking://user/"
		if !strings.HasPrefix(uri, prefix) {
			continue
		}
		rest := uri[len(prefix):]
		space := rest
		if idx := strings.Index(rest, "/"); idx >= 0 {
			space = rest[:idx]
		}
		if space != "" && space != "memories" {
			return space
		}
	}
	return ""
}

// patchLanguageText returns the text used to detect the patch output
// language. It mirrors _patch_language_text.
func patchLanguageText(patches []PatchMergePatch) string {
	parts := make([]string, 0, len(patches))
	for _, patch := range patches {
		parts = append(parts, memoryFileLanguageText(patch.AfterFile)...)
	}
	return strings.Join(parts, "\n")
}

// memoryFileLanguageText returns the text used for language detection.
// It mirrors _memory_file_language_text.
func memoryFileLanguageText(file MemoryFile) []string {
	parts := make([]string, 0, 8)
	for k, v := range file.ExtraFields {
		if _, hidden := patchMergeSystemHiddenFields[k]; hidden {
			continue
		}
		if k == "memory_type" || k == "version" {
			continue
		}
		parts = append(parts, stringValues(v)...)
	}
	if file.Content != "" {
		parts = append(parts, file.Content)
	}
	return parts
}

// stringValues flattens v into a list of string values. It mirrors
// _string_values.
func stringValues(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, stringValues(item)...)
		}
		return out
	case map[string]any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, stringValues(item)...)
		}
		return out
	}
	return nil
}

// equalValue reports whether before and after are deeply equal. JSON
// representation is used for non-string values.
func equalValue(before, after any) bool {
	if before == nil && after == nil {
		return true
	}
	if before == nil || after == nil {
		return false
	}
	if bs, ok := before.(string); ok {
		if as, ok := after.(string); ok {
			return bs == as
		}
	}
	bdata, _ := json.Marshal(before)
	adata, _ := json.Marshal(after)
	return string(bdata) == string(adata)
}
