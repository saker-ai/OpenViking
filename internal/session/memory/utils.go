// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// StripLinks removes relative markdown links from content, keeping only
// the link text. External, anchor, and absolute-path links are
// preserved. It mirrors LinkRenderer.strip_links.
func StripLinks(content string) string {
	return relativeLinkRE.ReplaceAllStringFunc(content, func(match string) string {
		m := relativeLinkRE.FindStringSubmatch(match)
		if len(m) < 3 {
			return match
		}
		target := m[2]
		if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "/") || strings.Contains(target, "://") {
			return match
		}
		return m[1]
	})
}

// StripAllLinks removes all markdown links regardless of target scheme.
func StripAllLinks(content string) string {
	return relativeLinkRE.ReplaceAllStringFunc(content, func(match string) string {
		m := relativeLinkRE.FindStringSubmatch(match)
		if len(m) < 3 {
			return match
		}
		return m[1]
	})
}

// containsCJK reports whether text contains any CJK ideograph.
func containsCJK(text string) bool { return cjkRE.MatchString(text) }

// isASCIIWordChar reports whether the first rune of char is [A-Za-z0-9_].
func isASCIIWordChar(char string) bool {
	if char == "" {
		return false
	}
	return asciiWordCharRE.MatchString(char)
}

// findMatchSpan returns the (start, end) byte offsets of matchText in
// content, or (-1, -1) when not found. For CJK matchText the search is
// case-sensitive; otherwise it is case-insensitive and the match must
// not be embedded inside a larger ASCII word.
func findMatchSpan(content, matchText string) (int, int) {
	if matchText == "" {
		return -1, -1
	}
	if containsCJK(matchText) {
		idx := strings.Index(content, matchText)
		if idx < 0 {
			return -1, -1
		}
		return idx, idx + len(matchText)
	}
	// Case-insensitive whole-word search.
	lowered := strings.ToLower(content)
	target := strings.ToLower(matchText)
	start := 0
	for {
		idx := strings.Index(lowered[start:], target)
		if idx < 0 {
			return -1, -1
		}
		abs := start + idx
		leftChar := ""
		if abs > 0 {
			r, _ := utf8.DecodeLastRuneInString(content[:abs])
			leftChar = string(r)
		}
		rightChar := ""
		end := abs + len(target)
		if end < len(content) {
			r, _ := utf8.DecodeRuneInString(content[end:])
			rightChar = string(r)
		}
		if !isASCIIWordChar(leftChar) && !isASCIIWordChar(rightChar) {
			return abs, end
		}
		start = end
	}
}

// relativePath computes a relative path from sourceURI to targetURI in
// the viking:// namespace. Returns "" when the URIs are in incompatible
// scopes. It mirrors LinkRenderer.relative_path.
func relativePath(sourceURI, targetURI string) string {
	src := uriParts(sourceURI)
	tgt := uriParts(targetURI)
	if len(src) == 0 || len(tgt) == 0 {
		return ""
	}
	if src[0] != tgt[0] {
		return ""
	}
	if len(src) < 2 || len(tgt) < 2 || src[1] != tgt[1] {
		return ""
	}
	common := 0
	for i := 0; i < len(src) && i < len(tgt); i++ {
		if src[i] == tgt[i] {
			common++
		} else {
			break
		}
	}
	if common < 1 {
		return ""
	}
	upCount := len(src) - common - 1
	downParts := tgt[common:]
	if upCount == 0 {
		if len(downParts) == 0 {
			return "./"
		}
		return strings.Join(downParts, "/")
	}
	upParts := make([]string, upCount)
	for i := range upParts {
		upParts[i] = ".."
	}
	return strings.Join(append(upParts, downParts...), "/")
}

// uriParts splits a viking:// URI into its slash-separated path
// segments, dropping the scheme. Returns nil for empty/invalid input.
// It mirrors openviking.core.namespace.uri_parts.
func uriParts(uri string) []string {
	if uri == "" {
		return nil
	}
	uri = strings.TrimPrefix(uri, "viking://")
	uri = strings.TrimPrefix(uri, "viking:")
	uri = strings.Trim(uri, "/")
	if uri == "" {
		return nil
	}
	parts := strings.Split(uri, "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RenderLinks replaces match_text in content with relative markdown
// links. Eligible links are sorted by weight descending; overlapping
// replacements are skipped. It mirrors LinkRenderer.render_links.
func RenderLinks(content, sourceURI string, links []map[string]any) string {
	eligible := make([]map[string]any, 0, len(links))
	for _, l := range links {
		if v, ok := l["match_text"].(string); ok && v != "" {
			eligible = append(eligible, l)
		}
	}
	if len(eligible) == 0 {
		return content
	}
	sortLinksByWeightDesc(eligible)

	type replacement struct {
		start, end int
		text       string
	}
	var reps []replacement
	for _, link := range eligible {
		matchText, _ := link["match_text"].(string)
		toURI, _ := link["to_uri"].(string)
		if toURI == sourceURI {
			continue
		}
		rel := relativePath(sourceURI, toURI)
		if rel == "" {
			rel = toURI
		}
		start, end := findMatchSpan(content, matchText)
		if start < 0 {
			continue
		}
		overlap := false
		for _, r := range reps {
			if !(end <= r.start || start >= r.end) {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}
		reps = append(reps, replacement{
			start: start,
			end:   end,
			text:  "[" + content[start:end] + "](" + rel + ")",
		})
	}
	// Apply in reverse order to preserve indices.
	for i := len(reps) - 1; i >= 0; i-- {
		r := reps[i]
		content = content[:r.start] + r.text + content[r.end:]
	}
	return content
}

// sortLinksByWeightDesc sorts links by weight (descending). Sort is
// stable to preserve insertion order for ties.
func sortLinksByWeightDesc(links []map[string]any) {
	for i := 1; i < len(links); i++ {
		for j := i; j > 0; j-- {
			w1, _ := links[j]["weight"].(float64)
			w2, _ := links[j-1]["weight"].(float64)
			if w1 > w2 {
				links[j], links[j-1] = links[j-1], links[j]
			} else {
				break
			}
		}
	}
}

// ParseMemoryFileWithFields parses memory file content with an optional
// MEMORY_FIELDS HTML comment. It mirrors
// openviking.session.memory.utils.messages.parse_memory_file_with_fields.
func ParseMemoryFileWithFields(content string) map[string]any {
	result := map[string]any{}
	if content == "" {
		result["content"] = ""
		return result
	}
	pattern := regexp.MustCompile(`<!--\s*MEMORY_FIELDS\s*([\s\S]*?)\s*-->`)
	match := pattern.FindStringSubmatchIndex(content)
	if match != nil {
		fieldsJSON := strings.TrimSpace(content[match[2]:match[3]])
		if fieldsJSON != "" {
			var parsed any
			if err := json.Unmarshal([]byte(fieldsJSON), &parsed); err == nil {
				switch v := parsed.(type) {
				case map[string]any:
					for k, val := range v {
						result[k] = val
					}
				case []any:
					if len(v) > 0 {
						if m, ok := v[0].(map[string]any); ok {
							for k, val := range m {
								result[k] = val
							}
						}
					}
				}
			}
		}
		// Remove the comment from content.
		content = strings.TrimSpace(pattern.ReplaceAllString(content, ""))
	}
	result["content"] = content
	return result
}

// MemoryVersionFromFields returns the positive MEMORY_FIELDS version, or
// default when absent/invalid.
func MemoryVersionFromFields(fields map[string]any, defaultVersion int) int {
	if defaultVersion <= 0 {
		defaultVersion = 1
	}
	if fields == nil {
		return defaultVersion
	}
	v, ok := fields["version"]
	if !ok {
		return defaultVersion
	}
	i, ok := asInt(v)
	if !ok || i <= 0 {
		return defaultVersion
	}
	return i
}

// NextMemoryVersion returns the next persisted MEMORY_FIELDS version.
func NextMemoryVersion(oldFile *MemoryFile) int {
	if oldFile == nil {
		return 1
	}
	return MemoryVersionFromFields(oldFile.ExtraFields, 1) + 1
}

// BumpMemoryVersion increments a MemoryFile's persisted version in place.
func BumpMemoryVersion(mf *MemoryFile) {
	if mf == nil {
		return
	}
	if mf.ExtraFields == nil {
		mf.ExtraFields = map[string]any{}
	}
	mf.ExtraFields["version"] = MemoryVersionFromFields(mf.ExtraFields, 1) + 1
}

// uriBasename returns the URI basename without the .md suffix.
func uriBasename(uri string) string {
	if uri == "" {
		return ""
	}
	name := uri
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	return strings.TrimSuffix(name, ".md")
}

// templateLinkTarget returns the relative path from sourceURI to
// targetURI, or targetURI itself when sourceURI is empty.
func templateLinkTarget(sourceURI, targetURI string) string {
	if sourceURI != "" && targetURI != "" {
		if rel := relativePath(sourceURI, targetURI); rel != "" {
			return rel
		}
	}
	return targetURI
}

// serializeWithMetadata serialises a metadata map plus optional content
// template into the markdown-plus-MEMORY_FIELDS format. It mirrors the
// Python _serialize_with_metadata helper.
func serializeWithMetadata(metadata map[string]any, contentTemplate string, sourceURI string) string {
	content, _ := metadata["content"].(string)
	delete(metadata, "content")

	if contentTemplate != "" {
		vars := map[string]any{}
		for k, v := range metadata {
			vars[k] = v
		}
		vars["content"] = content
		if _, ok := vars["links"]; !ok {
			vars["links"] = []map[string]any{}
		}
		if _, ok := vars["backlinks"]; !ok {
			vars["backlinks"] = []map[string]any{}
		}
		vars["source_uri"] = sourceURI
		vars["uri_basename"] = uriBasename
		vars["link_target"] = func(targetURI string) string {
			return templateLinkTarget(sourceURI, targetURI)
		}
		if rendered, err := RenderTemplate(contentTemplate, vars); err == nil {
			content = rendered
		}
	}

	cleaned := map[string]any{}
	for k, v := range metadata {
		if v == nil {
			continue
		}
		cleaned[k] = v
	}
	delete(cleaned, "_uri")

	if len(cleaned) == 0 {
		return content
	}

	if links, ok := cleaned["links"].([]map[string]any); ok && sourceURI != "" {
		content = RenderLinks(content, sourceURI, links)
	}

	metadataJSON, err := json.MarshalIndent(cleaned, "", "  ")
	if err != nil {
		return content
	}
	comment := "\n\n<!-- MEMORY_FIELDS\n" + string(metadataJSON) + "\n-->"
	if strings.TrimSpace(content) == "" {
		return strings.TrimLeft(comment, "\n")
	}
	return content + comment
}

// MemoryFileRead parses raw content into a MemoryFile. It mirrors
// MemoryFileUtils.read.
func MemoryFileRead(rawContent, uri string) MemoryFile {
	parsed := ParseMemoryFileWithFields(rawContent)
	deserializeDatetimes(parsed)
	return FromParsed(uri, parsed)
}

// MemoryFileWrite serialises a MemoryFile into the markdown +
// MEMORY_FIELDS format. It mirrors MemoryFileUtils.write.
func MemoryFileWrite(mf MemoryFile, contentTemplate string) string {
	metadata := mf.ToMetadata()
	return serializeWithMetadata(metadata, contentTemplate, mf.URI)
}

// TruncateContent truncates content to maxChars while keeping complete
// lines. It mirrors MemoryFileUtils.truncate_content.
func TruncateContent(content string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 1000
	}
	if len(content) <= maxChars {
		return content
	}
	truncated := content[:maxChars]
	if idx := strings.LastIndex(truncated, "\n"); idx > 0 {
		truncated = truncated[:idx]
	}
	return truncated + "\n... [truncated " + intToStr(len(content)-len(truncated)) + " chars]"
}

// deserializeDatetimes parses ISO-8601 strings for created_at/updated_at
// back into time.Time values inside metadata in place.
func deserializeDatetimes(metadata map[string]any) {
	for _, key := range []string{"created_at", "updated_at"} {
		v, ok := metadata[key]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			metadata[key] = t
		}
	}
}

// intToStr renders n as a decimal string.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// RenderTemplate is the Jinja2-equivalent template renderer. It supports
// {{ variable }} substitution and a few helper functions registered via
// vars. It mirrors openviking.session.memory.utils.template_utils.TemplateUtils.render
// for the subset of features the memory subsystem uses.
//
// Python-only Jinja2 features (control flow, filters, expressions) are
// not supported. Templates that use them should be pre-rendered in
// Python or replaced with Go templates by callers.
func RenderTemplate(template string, vars map[string]any) (string, error) {
	out := template
	// Replace {{ var }} tokens. Function values are not invoked here
	// (only string substitutions are supported). Unknown variables are
	// replaced with empty string, mirroring Jinja2 Undefined behaviour
	// for the memory templates' simple substitutions.
	for {
		idx := strings.Index(out, "{{")
		if idx < 0 {
			break
		}
		end := strings.Index(out[idx:], "}}")
		if end < 0 {
			break
		}
		end += idx
		expr := strings.TrimSpace(out[idx+2 : end])
		// Support only bare variable names; dot/attribute access is not
		// implemented.
		var replacement string
		if v, ok := vars[expr]; ok {
			switch x := v.(type) {
			case string:
				replacement = x
			case func(string) string:
				// Functions like uri_basename / link_target are called
				// with no argument in our templates; we leave them as-is.
				_ = x
			default:
				if b, err := json.Marshal(x); err == nil {
					replacement = string(b)
				}
			}
		}
		out = out[:idx] + replacement + out[end+2:]
	}
	return strings.TrimSpace(out), nil
}

// GenerateURI renders a memory type's URI template. It mirrors
// openviking.session.memory.utils.uri.generate_uri.
func GenerateURI(mt MemoryTypeSchema, fields map[string]any, userSpace string) (string, error) {
	if userSpace == "" {
		userSpace = "default"
	}
	dirTmpl := mt.Directory
	fileTmpl := mt.FilenameTemplate
	var uriTmpl string
	if dirTmpl != "" && fileTmpl != "" {
		uriTmpl = strings.TrimRight(dirTmpl, "/") + "/" + strings.TrimLeft(fileTmpl, "/")
	} else if dirTmpl != "" {
		uriTmpl = dirTmpl
	} else {
		uriTmpl = fileTmpl
	}
	context := map[string]any{"user_space": userSpace}
	for k, v := range fields {
		context[k] = v
	}
	// Validate required variables.
	required := extractTemplateVars(uriTmpl)
	for _, v := range required {
		val, ok := context[v]
		if !ok {
			return "", NewPatchParseError("missing template variable: " + v)
		}
		if val == nil {
			return "", NewPatchParseError("template variable '" + v + "' has nil value")
		}
	}
	return RenderTemplate(uriTmpl, context)
}

// ValidateURITemplate reports whether a memory type's URI template is
// well-formed. It mirrors openviking.session.memory.utils.uri.validate_uri_template.
func ValidateURITemplate(mt MemoryTypeSchema) bool {
	if mt.Directory == "" && mt.FilenameTemplate == "" {
		return false
	}
	if mt.FilenameTemplate == "" {
		return true
	}
	fieldNames := map[string]struct{}{}
	for _, f := range mt.Fields {
		fieldNames[f.Name] = struct{}{}
	}
	required := extractTemplateVars(mt.FilenameTemplate)
	for _, v := range required {
		if v == "user_space" {
			continue
		}
		if _, ok := fieldNames[v]; !ok {
			return false
		}
	}
	return true
}

// extractTemplateVars returns the list of {{ variable }} names in tmpl.
func extractTemplateVars(tmpl string) []string {
	var out []string
	for {
		idx := strings.Index(tmpl, "{{")
		if idx < 0 {
			break
		}
		end := strings.Index(tmpl[idx:], "}}")
		if end < 0 {
			break
		}
		end += idx
		name := strings.TrimSpace(tmpl[idx+2 : end])
		if name != "" {
			out = append(out, name)
		}
		tmpl = tmpl[end+2:]
	}
	return out
}

// patternMatchesURI reports whether uri matches a pattern with
// {{ variable }} placeholders or * / ** wildcards.
func patternMatchesURI(pattern, uri string) bool {
	// Convert pattern to regex.
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		c := pattern[i]
		switch {
		case c == '{' && i+1 < len(pattern) && pattern[i+1] == '{':
			// {{ variable }} -> [^/]+
			end := strings.Index(pattern[i:], "}}")
			if end < 0 {
				b.WriteString(regexpQuoteMeta(pattern[i:]))
				break
			}
			b.WriteString("[^/]+")
			i += end + 2
		case c == '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i += 2
			} else {
				b.WriteString("[^/]*")
				i++
			}
		default:
			// Escape regex metacharacter.
			if strings.IndexByte(`\.+*?()|[]{}^$`, c) >= 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
			i++
		}
	}
	b.WriteString("$")
	matched, err := regexp.MatchString(b.String(), uri)
	if err != nil {
		return false
	}
	return matched
}

// regexpQuoteMeta escapes regex metacharacters in s. We avoid importing
// regexp's QuoteMeta to keep the helper explicit.
func regexpQuoteMeta(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(`\.+*?()|[]{}^$`, c) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// IsURIAllowed reports whether uri falls under any allowed directory or
// pattern. It mirrors openviking.session.memory.utils.uri.is_uri_allowed.
func IsURIAllowed(uri string, allowedDirs, allowedPatterns []string) bool {
	for _, dir := range allowedDirs {
		if uri == dir || strings.HasPrefix(uri, dir+"/") {
			return true
		}
	}
	for _, pattern := range allowedPatterns {
		if patternMatchesURI(pattern, uri) {
			return true
		}
	}
	return false
}

// ExtractJSONContent extracts the JSON content from an LLM response by
// trimming leading/trailing non-JSON. It mirrors
// openviking.session.memory.utils.json_parser.extract_json_content.
func ExtractJSONContent(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	first := strings.IndexAny(s, "{[")
	if first < 0 {
		return s
	}
	temp := s[first:]
	lastBrace := strings.LastIndex(temp, "}")
	lastBracket := strings.LastIndex(temp, "]")
	end := -1
	if lastBrace > lastBracket {
		end = lastBrace
	} else {
		end = lastBracket
	}
	if end < 0 {
		return s
	}
	temp = temp[:end+1]
	out := strings.TrimSpace(temp)
	if out == "" {
		return s
	}
	return out
}

// ParseJSONWithStability is the Go counterpart of the five-layer
// parse_json_with_stability helper. Layer 1 (extract) and Layer 2
// (repair-via-tolerant-parse) are implemented; Layers 3-5 (model
// validation with fault tolerance) are not because Go has no equivalent
// of pydantic.TypeAdapter. Callers should validate the returned
// map[string]any with their own schema logic.
//
// Returns (parsed, nil) on success, or (nil, error) on failure.
func ParseJSONWithStability(content string) (map[string]any, error) {
	if content == "" {
		return nil, errors.New("empty content")
	}
	cleaned := ExtractJSONContent(content)
	if cleaned == "" {
		return nil, errors.New("no JSON content found after cleanup")
	}
	var parsed any
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil, fmt.Errorf("JSON parsing failed: %w", err)
	}
	// Layer 3: list -> object extraction.
	if list, ok := parsed.([]any); ok && len(list) > 0 {
		if m, ok := list[0].(map[string]any); ok {
			parsed = m
		} else {
			return nil, errors.New("expected dict after parsing, got list with non-object first element")
		}
	} else if _, ok := parsed.(map[string]any); !ok {
		return nil, errors.New("expected dict after parsing")
	}
	m, _ := parsed.(map[string]any)
	return m, nil
}

// AddToolCallPairToMessages appends a tool-call pair (as a user message
// containing the JSON-encoded {tool_call_name, args, result}) to
// messages. It mirrors openviking.session.memory.tools.add_tool_call_pair_to_messages.
func AddToolCallPairToMessages(messages []map[string]any, callID any, toolName string, params, result any) []map[string]any {
	payload := map[string]any{
		"tool_call_name": toolName,
		"args":           params,
		"result":         result,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return messages
	}
	return append(messages, map[string]any{
		"role":    "user",
		"content": string(body),
	})
}

// OptimizeSearchResult trims and filters a search result to reduce
// token consumption. It mirrors
// openviking.session.memory.tools.optimize_search_result.
func OptimizeSearchResult(result any, limit int) any {
	if limit <= 0 {
		limit = 10
	}
	m, ok := result.(map[string]any)
	if !ok {
		return []any{}
	}
	if _, ok := m["error"]; ok {
		errMsg, _ := m["error"].(string)
		return map[string]any{"error": ExtractErrorSummary(errMsg)}
	}
	memories, ok := m["memories"].([]any)
	if !ok {
		return []any{}
	}
	out := []any{}
	for _, item := range memories {
		im, ok := item.(map[string]any)
		if !ok {
			continue
		}
		uri, _ := im["uri"].(string)
		if strings.HasSuffix(uri, ".abstract.md") || strings.HasSuffix(uri, ".overview.md") {
			continue
		}
		score, _ := im["score"]
		out = append(out, map[string]any{"uri": uri, "score": score})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ExtractErrorSummary shortens an error message to a stable prefix.
func ExtractErrorSummary(errStr string) string {
	if strings.Contains(errStr, "File not found") {
		return "File not found"
	}
	if strings.Contains(errStr, "Permission denied") {
		return "Permission denied"
	}
	if strings.Contains(errStr, "Timeout") {
		return "Timeout"
	}
	if len(errStr) > 50 {
		return errStr[:50]
	}
	return errStr
}

// OptimizeToolResult applies the right optimizer based on toolName. It
// mirrors openviking.session.memory.tools.optimize_tool_result.
func OptimizeToolResult(toolName string, result any) any {
	if m, ok := result.(map[string]any); ok {
		if _, ok := m["error"]; ok {
			errMsg, _ := m["error"].(string)
			m["error"] = ExtractErrorSummary(errMsg)
			return m
		}
		if toolName == "search" {
			return OptimizeSearchResult(result, 10)
		}
		if toolName == "read" {
			if content, ok := m["content"].(string); ok {
				m["content"] = TruncateContent(content, 1000)
			}
		}
	}
	return result
}

// FormatSize formats a byte count as a human-readable string.
func FormatSize(sizeBytes int64) string {
	if sizeBytes >= 1024*1024 {
		return formatFloat(float64(sizeBytes)/(1024*1024)) + "M"
	}
	if sizeBytes >= 1024 {
		return formatFloat(float64(sizeBytes)/1024) + "K"
	}
	return intToStr(int(sizeBytes)) + "B"
}

// formatFloat renders f with one decimal place.
func formatFloat(f float64) string {
	whole := int(f)
	frac := int((f - float64(whole)) * 10)
	if frac < 0 {
		frac = -frac
	}
	if whole == 0 && f < 0 {
		return "-0." + intToStr(frac)
	}
	return intToStr(whole) + "." + intToStr(frac)
}

// ResolveOutputLanguageFromText detects the dominant language of text
// with an explicit fallback. It mirrors
// openviking.session.memory.utils.language.resolve_output_language_from_text.
func ResolveOutputLanguageFromText(text, fallbackLanguage string) string {
	if fallbackLanguage == "" {
		fallbackLanguage = "en"
	}
	return detectLanguageFromText(text, fallbackLanguage)
}

// ResolveOutputLanguage detects the dominant language of text. The
// system-locale fallback logic is intentionally conservative: when no
// signal is present, English is returned.
func ResolveOutputLanguage(text string) string {
	return ResolveOutputLanguageFromText(text, "en")
}

// DetectLanguageFromConversation scopes detection to user-role
// messages. It mirrors
// openviking.session.memory.utils.language.detect_language_from_conversation.
func DetectLanguageFromConversation(conversation, fallbackLanguage string) string {
	if fallbackLanguage == "" {
		fallbackLanguage = "en"
	}
	var userLines []string
	for _, line := range strings.Split(conversation, "\n") {
		stripped := strings.TrimSpace(line)
		lower := strings.ToLower(stripped)
		if strings.HasPrefix(lower, "[user]:") || strings.HasPrefix(lower, "user:") {
			content := stripped
			if idx := strings.Index(stripped, ":"); idx >= 0 {
				content = strings.TrimSpace(stripped[idx+1:])
			}
			if content != "" {
				userLines = append(userLines, content)
			}
			continue
		}
		if idx := strings.Index(stripped, ":"); idx >= 0 {
			header := strings.ToLower(stripped[:idx])
			if strings.Contains(header, "[user]") || strings.Contains(header, "user") {
				content := strings.TrimSpace(stripped[idx+1:])
				if content != "" {
					userLines = append(userLines, content)
				}
			}
		}
	}
	text := strings.Join(userLines, "\n")
	if text == "" {
		text = conversation
	}
	return detectLanguageFromText(text, fallbackLanguage)
}

// stripLanguageDetectionNoise removes URI-like machine tokens.
func stripLanguageDetectionNoise(text string) string {
	return uriLanguageNoiseRE.ReplaceAllString(text, " ")
}

// detectLanguageFromText detects the dominant language from text.
// It is the Go counterpart of
// openviking.session.memory.utils.language._detect_language_from_text.
func detectLanguageFromText(text, fallback string) string {
	if fallback == "" {
		fallback = "en"
	}
	text = stripLanguageDetectionNoise(text)
	if text == "" {
		return fallback
	}
	counts := map[string]int{
		"zh-CN":   countRunes(text, unicode.RangeTable{R16: []unicode.Range16{{Lo: 0x4e00, Hi: 0x9fff, Stride: 1}}}),
		"ja_kana": countRunes(text, unicode.RangeTable{R16: []unicode.Range16{{Lo: 0x3040, Hi: 0x30ff, Stride: 1}}}),
		"ko":      countRunes(text, unicode.RangeTable{R16: []unicode.Range16{{Lo: 0xac00, Hi: 0xd7af, Stride: 1}}}),
		"ru":      countRunes(text, unicode.RangeTable{R16: []unicode.Range16{{Lo: 0x0400, Hi: 0x04ff, Stride: 1}}}),
		"ar":      countRunes(text, unicode.RangeTable{R16: []unicode.Range16{{Lo: 0x0600, Hi: 0x06ff, Stride: 1}}}),
		"latin":   countLatinRunes(text),
	}
	signalTotal := 0
	for _, c := range counts {
		signalTotal += c
	}
	if signalTotal == 0 {
		return fallback
	}

	if counts["ja_kana"] >= 3 {
		return "ja"
	}

	nonLatin := map[string]int{
		"zh-CN": counts["zh-CN"],
		"ko":    counts["ko"],
		"ru":    counts["ru"],
		"ar":    counts["ar"],
	}
	bestLang := "zh-CN"
	bestScore := -1
	for lang, score := range nonLatin {
		if score > bestScore {
			bestScore = score
			bestLang = lang
		}
	}
	if bestScore >= 2 && float64(bestScore)/float64(signalTotal) >= 0.2 {
		return bestLang
	}
	if counts["latin"] > 0 {
		return "en"
	}
	return fallback
}

// countRunes counts the runes in text that fall in the given range
// table. We use a hand-rolled scanner because unicode.Is(rg, r) only
// handles pre-defined range tables.
func countRunes(text string, rg unicode.RangeTable) int {
	count := 0
	for _, r := range text {
		for _, rng := range rg.R16 {
			if r >= rune(rng.Lo) && r <= rune(rng.Hi) {
				count++
				break
			}
		}
	}
	return count
}

// countLatinRunes counts ASCII letters and common Latin-1 supplement
// letters.
func countLatinRunes(text string) int {
	count := 0
	for _, r := range text {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			count++
			continue
		}
		if r >= 0x00c0 && r <= 0x024f {
			count++
		}
	}
	return count
}
