// Package skill — session-derived skill operation helpers.
//
// The two pieces ported from Python:
//   - dedup.go: lightweight batch dedup for session skill operations
//     (openviking/session/skill/dedup.py).
//   - updater.go: stub for SkillOperationUpdater (depends on the full
//     session/memory/ pipeline which lands incrementally with P0-2).
//
// ResolvedOperation and ResolvedOperations are imported from
// internal/session/memory/ which is the canonical home for those types.
package skill

import (
	"regexp"
	"strings"

	sessionmemory "github.com/saker-ai/ctxhub/internal/session/memory"
)

// ResolvedOperation is an alias for the canonical type in
// internal/session/memory/. It is kept here so existing call sites in
// the skill package compile without churning their import list.
type ResolvedOperation = sessionmemory.ResolvedOperation

// ResolvedOperations is an alias for the canonical type in
// internal/session/memory/.
type ResolvedOperations = sessionmemory.ResolvedOperations

var whitespaceRE = regexp.MustCompile(`\s+`)

// DedupOperations removes duplicate session_skill upserts within a batch.
// Two operations are duplicates when their normalized content bodies match.
// Operations without a content body (or with old_memory_file_content set)
// are passed through unchanged.
//
// Returns a new ResolvedOperations; the input is not mutated.
func DedupOperations(in ResolvedOperations) ResolvedOperations {
	seen := make(map[string]int)
	deduped := make([]ResolvedOperation, 0, len(in.UpsertOperations))
	duplicates := 0

	for _, op := range in.UpsertOperations {
		sig := buildSessionSkillSignature(op)
		if sig == "" {
			deduped = append(deduped, op)
			continue
		}
		if existingIdx, ok := seen[sig]; ok {
			duplicates++
			_ = existingIdx // kept op index; logger would go here
			continue
		}
		seen[sig] = len(deduped)
		deduped = append(deduped, op)
	}

	if duplicates == 0 {
		return in
	}
	return ResolvedOperations{
		UpsertOperations:   deduped,
		DeleteFileContents: in.DeleteFileContents,
		Errors:             in.Errors,
		ResolvedLinks:      in.ResolvedLinks,
		DeleteReplacements: in.DeleteReplacements,
	}
}

// buildSessionSkillSignature returns the dedup signature for an op, or ""
// when the op is not a session_skills upsert or has no usable body.
func buildSessionSkillSignature(op ResolvedOperation) string {
	if op.MemoryType != "session_skills" || op.OldMemoryFileContent != nil {
		return ""
	}
	body := extractFullBody(op.MemoryFields["content"])
	if body == "" {
		return ""
	}
	normalized := normalizeText(body)
	if normalized == "" {
		return ""
	}
	return "session_skill_body:" + normalized
}

// extractFullBody pulls the canonical body text from a content field. The
// content may be a plain string or a {blocks: [{search, replace}]} dict
// (the merge-op shape used by the memory pipeline).
func extractFullBody(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case map[string]any:
		blocks, ok := v["blocks"].([]any)
		if !ok || len(blocks) != 1 {
			return ""
		}
		block, ok := blocks[0].(map[string]any)
		if !ok {
			return ""
		}
		search, _ := block["search"].(string)
		replace, _ := block["replace"].(string)
		if strings.TrimSpace(search) != "" || strings.TrimSpace(replace) == "" {
			return ""
		}
		return replace
	}
	return ""
}

// normalizeText collapses whitespace and casefolds so signatures are
// robust to formatting differences.
func normalizeText(s string) string {
	return strings.TrimSpace(whitespaceRE.ReplaceAllString(s, " "))
}

// SkillName extracts the skill_name from op.MemoryFields, returning
// "<unknown>" when absent. Used in logs.
func SkillName(op ResolvedOperation) string {
	if name, ok := op.MemoryFields["skill_name"].(string); ok && strings.TrimSpace(name) != "" {
		return name
	}
	return "<unknown>"
}
