package privacy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// This file ports the Python openviking/privacy/skill_extractor.py,
// skill_placeholder.py, and skill_restore.py to Go.
//
// The Python skill_extractor.py uses an LLM (VLM) to identify
// sensitive values inside skill content (key-value pairs, JSON/YAML
// blocks, prose with secrets). The Go version uses a regex
// approximation: the same PII regexes from extractor.go are applied
// to the skill content to find candidate values. This is a documented
// gap — the LLM would assign semantic field names (e.g., "user_email")
// while the regex approximation assigns sequential names ("field_1",
// "field_2"). The placeholderize/restore logic is a faithful port.

// SkillPrivacyExtractionResult mirrors the Python dataclass. It
// captures the original content, the sanitized content with
// placeholders, and the bidirectional mapping between them.
type SkillPrivacyExtractionResult struct {
	// Values is the field_name -> original_value map (only fields
	// that were successfully replaced are included).
	Values map[string]string
	// OriginalContent is the input content unchanged.
	OriginalContent string
	// SanitizedContent is the content with PII values replaced by
	// placeholders.
	SanitizedContent string
	// OriginalContentBlocks lists the original values that were
	// replaced, in replacement order.
	OriginalContentBlocks []string
	// ReplacementContentBlocks lists the placeholders that replaced
	// the values, in replacement order. Indexes line up with
	// OriginalContentBlocks.
	ReplacementContentBlocks []string
}

// SkillPrivacyPlaceholderizationResult mirrors the Python dataclass.
type SkillPrivacyPlaceholderizationResult struct {
	SanitizedContent         string
	OriginalContentBlocks    []string
	ReplacementContentBlocks []string
	ReplacedValues           map[string]string
}

// skillPlaceholderTokenRE matches any {{ov_privacy:skill:NAME:FIELD}}
// placeholder produced by BuildSkillPlaceholder. Used by
// RestoreSkillContent.
var skillPlaceholderTokenRE = regexp.MustCompile(`\{\{ov_privacy:skill:([^:{}]+):([^:{}]+)\}\}`)

// BuildSkillPlaceholder returns the placeholder string for a skill
// field, mirroring the Python build_placeholder(skill_name, field_name).
//
// Format: {{ov_privacy:skill:<skill_name>:<field_name>}}
//
// skill_name and field_name are taken verbatim; callers should ensure
// they don't contain ":" or "{}" characters.
func BuildSkillPlaceholder(skillName, fieldName string) string {
	return fmt.Sprintf("{{ov_privacy:skill:%s:%s}}", skillName, fieldName)
}

// PlaceholderizeSkillContentWithBlocks replaces the given values in
// content with skill-keyed placeholders. Mirrors the Python
// placeholderize_skill_content_with_blocks.
//
// For each (field_name, raw_value) pair, the function tries a series
// of structured replacements (quoted, key-value, assignment) so that
// common YAML/JSON/ini patterns are covered. Only fields whose value
// was actually replaced are added to ReplacedValues.
//
// Values are processed in descending length order so longer values
// are replaced before shorter ones (avoids partial-replace bugs).
func PlaceholderizeSkillContentWithBlocks(content, skillName string, values map[string]string) *SkillPrivacyPlaceholderizationResult {
	res := &SkillPrivacyPlaceholderizationResult{
		SanitizedContent:         content,
		OriginalContentBlocks:    []string{},
		ReplacementContentBlocks: []string{},
		ReplacedValues:           map[string]string{},
	}
	// Sort entries by descending value length so longer values are
	// replaced first.
	type entry struct {
		field string
		value string
	}
	entries := make([]entry, 0, len(values))
	for f, v := range values {
		entries = append(entries, entry{field: f, value: v})
	}
	sort.Slice(entries, func(i, j int) bool {
		if len(entries[i].value) != len(entries[j].value) {
			return len(entries[i].value) > len(entries[j].value)
		}
		return entries[i].field < entries[j].field
	})
	for _, e := range entries {
		if e.value == "" {
			continue
		}
		placeholder := BuildSkillPlaceholder(skillName, e.field)
		newContent, replaced := replaceStructuredValue(res.SanitizedContent, e.value, placeholder)
		if replaced {
			res.SanitizedContent = newContent
			res.OriginalContentBlocks = append(res.OriginalContentBlocks, e.value)
			res.ReplacementContentBlocks = append(res.ReplacementContentBlocks, placeholder)
			res.ReplacedValues[e.field] = e.value
		}
	}
	return res
}

// PlaceholderizeSkillContent is the simple wrapper that returns only
// the sanitized content. Mirrors the Python placeholderize_skill_content.
func PlaceholderizeSkillContent(content, skillName string, values map[string]string) string {
	return PlaceholderizeSkillContentWithBlocks(content, skillName, values).SanitizedContent
}

// replaceStructuredValue tries a series of structured replacements
// (quoted, key-value, assignment) and returns the modified content
// plus a flag indicating whether any replacement happened. Mirrors
// the Python _replace_structured_value.
func replaceStructuredValue(content, rawValue, placeholder string) (string, bool) {
	// Order matters: more specific (with separators/newlines) first
	// so we don't accidentally partial-replace.
	replacements := []struct {
		old string
		new string
	}{
		{`"` + rawValue + `"`, `"` + placeholder + `"`},
		{"'" + rawValue + "'", "'" + placeholder + "'"},
		{": " + rawValue + "\n", ": " + placeholder + "\n"},
		{": " + rawValue + "\r\n", ": " + placeholder + "\r\n"},
		{":" + rawValue + "\n", ":" + placeholder + "\n"},
		{":" + rawValue + "\r\n", ":" + placeholder + "\r\n"},
		{": " + rawValue, ": " + placeholder},
		{":" + rawValue, ":" + placeholder},
		{"= " + rawValue + "\n", "= " + placeholder + "\n"},
		{"= " + rawValue + "\r\n", "= " + placeholder + "\r\n"},
		{"=" + rawValue + "\n", "=" + placeholder + "\n"},
		{"=" + rawValue + "\r\n", "=" + placeholder + "\r\n"},
		{"= " + rawValue, "= " + placeholder},
		{"=" + rawValue, "=" + placeholder},
	}
	replaced := false
	for _, r := range replacements {
		if strings.Contains(content, r.old) {
			content = strings.ReplaceAll(content, r.old, r.new)
			replaced = true
		}
	}
	return content, replaced
}

// ExtractSkillPrivacyValues extracts PII values from skill content
// using a regex approximation of the Python LLM-based extraction.
//
// The Python version calls a VLM to identify sensitive values and
// assign semantic field names. The Go version uses the same PII
// regexes from extractor.go to find candidate values, assigns them
// sequential field names (field_1, field_2, ...), and runs them
// through PlaceholderizeSkillContentWithBlocks.
//
// Gap: semantic field names from the LLM are not preserved. The
// placeholderize + restore logic is a faithful port.
func ExtractSkillPrivacyValues(skillName, skillDescription, content string) *SkillPrivacyExtractionResult {
	matches := Extract(content)
	values := map[string]string{}
	// Assign field names in left-to-right order, deduplicating by
	// value so the same PII value gets one field name.
	seen := map[string]string{}
	counter := 0
	for _, m := range matches {
		if _, ok := seen[m.Value]; ok {
			continue
		}
		counter++
		fieldName := fmt.Sprintf("field_%d", counter)
		values[fieldName] = m.Value
		seen[m.Value] = fieldName
	}
	placeholderResult := PlaceholderizeSkillContentWithBlocks(content, skillName, values)
	return &SkillPrivacyExtractionResult{
		Values:                   placeholderResult.ReplacedValues,
		OriginalContent:          content,
		SanitizedContent:         placeholderResult.SanitizedContent,
		OriginalContentBlocks:    placeholderResult.OriginalContentBlocks,
		ReplacementContentBlocks: placeholderResult.ReplacementContentBlocks,
	}
}

// RestoreSkillContent reverses PlaceholderizeSkillContent, mirroring
// the Python restore_skill_content. Placeholders with no entry in
// values are surfaced in a [Privacy Config Notice] block appended to
// the content; values that were not referenced in the content are
// also surfaced.
func RestoreSkillContent(content, skillName string, values map[string]string) string {
	restored := content
	keys := extractPlaceholderKeys(content, skillName)
	keySet := map[string]bool{}
	for _, k := range keys {
		keySet[k] = true
	}

	var unresolved []string
	for _, fieldName := range keys {
		placeholder := BuildSkillPlaceholder(skillName, fieldName)
		rawValue, ok := values[fieldName]
		if !ok {
			unresolved = append(unresolved, fieldName+"=<missing>")
			continue
		}
		if rawValue == "" {
			unresolved = append(unresolved, fieldName+`=""`)
			continue
		}
		restored = strings.ReplaceAll(restored, placeholder, rawValue)
	}

	var extra []string
	for k, v := range values {
		if keySet[k] {
			continue
		}
		if v == "" {
			continue
		}
		extra = append(extra, k+"="+v)
	}
	sort.Strings(extra)

	if len(unresolved) > 0 || len(extra) > 0 {
		restored += "\n\n[Privacy Config Notice]\n"
		if len(unresolved) > 0 {
			restored += "Missing config: " + strings.Join(unresolved, ", ") + "\n"
		}
		if len(extra) > 0 {
			restored += "Configured but not referenced in content: " + strings.Join(extra, ", ") + "\n"
		}
	}
	return restored
}

// extractPlaceholderKeys returns the unique field names referenced by
// placeholders in content for the given skill, in order of first
// appearance. Mirrors the Python _extract_placeholder_keys.
func extractPlaceholderKeys(content, skillName string) []string {
	pattern := regexp.MustCompile(`\{\{ov_privacy:skill:` + regexp.QuoteMeta(skillName) + `:([^:}]+)\}\}`)
	keys := []string{}
	seen := map[string]bool{}
	for _, m := range pattern.FindAllStringSubmatch(content, -1) {
		if len(m) < 2 {
			continue
		}
		k := m[1]
		if seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys
}

// GetSkillNameFromURI extracts the skill name from a viking:// URI
// pointing at a skill's SKILL.md file. Mirrors the Python
// get_skill_name_from_uri. Returns "" if the URI is not a skill URI.
//
// Example: "viking://user/acme/skills/my-skill/SKILL.md" -> "my-skill".
func GetSkillNameFromURI(uri string) string {
	normalized := strings.TrimSpace(uri)
	normalized = strings.TrimSuffix(normalized, "/")
	marker := "/skills/"
	suffix := "/SKILL.md"
	if !strings.Contains(normalized, marker) || !strings.HasSuffix(normalized, suffix) {
		return ""
	}
	start := strings.LastIndex(normalized, marker)
	if start < 0 {
		return ""
	}
	middle := normalized[start+len(marker):]
	middle = strings.TrimSuffix(middle, suffix)
	if middle == "" || strings.Contains(middle, "/") {
		return ""
	}
	return middle
}
