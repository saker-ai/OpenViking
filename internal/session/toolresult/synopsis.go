// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

// Package toolresult implements deterministic synopsis generation for
// externalized tool results. It ports openviking/session/tool_result_synopsis.py
// to Go so the bot can replace large tool outputs with a small typed stub
// while keeping the raw bytes addressable by ref.
//
// The synopsis is intentionally lossy: it surfaces shape (JSON keys, table
// columns, code symbols, text headers) rather than full content. The stub
// rendered by RenderToolResultStub points callers at the persisted raw
// output via a ref URI.
package toolresult

import (
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// ToolResultKind enumerates the detected shape of a tool result body.
type ToolResultKind string

const (
	KindJSON    ToolResultKind = "json"
	KindCSV     ToolResultKind = "csv"
	KindTSV     ToolResultKind = "tsv"
	KindYAML    ToolResultKind = "yaml"
	KindXML     ToolResultKind = "xml"
	KindCode    ToolResultKind = "code"
	KindText    ToolResultKind = "text"
	KindUnknown ToolResultKind = "unknown"
)

const (
	textHeaderLimit      = 18
	textExcerptChars     = 500
	jsonMaxDepth         = 2
	jsonArraySampleLimit = 3
	jsonObjectKeyLimit   = 10
	yamlKeyLimit         = 30
	xmlChildTagLimit     = 30
	tableFirstRowSample  = 180
	codeImportLimit      = 12
	codeSymbolLimit      = 24
	codeImportLineChars  = 180
	codeSymbolLineChars  = 200
)

// ToolResultSynopsis is the deterministic, serializable shape of one tool
// result. It mirrors openviking.session.tool_result_synopsis.ToolResultSynopsis.
type ToolResultSynopsis struct {
	Kind         ToolResultKind `json:"kind"`
	Title        string         `json:"title"`
	Summary      []string       `json:"summary"`
	Structure    []string       `json:"structure"`
	NotableItems []string       `json:"notable_items"`
	Sample       string         `json:"sample"`
}

// ToMap returns a stringly-typed map suitable for JSON persistence. Slices
// are converted to []any so JSON round-trip (which loses the element type)
// and direct map hand-off both decode back via SynopsisFromMap.
func (s ToolResultSynopsis) ToMap() map[string]any {
	toAnySlice := func(in []string) []any {
		out := make([]any, 0, len(in))
		for _, s := range in {
			out = append(out, s)
		}
		return out
	}
	return map[string]any{
		"kind":          string(s.Kind),
		"title":         s.Title,
		"summary":       toAnySlice(s.Summary),
		"structure":     toAnySlice(s.Structure),
		"notable_items": toAnySlice(s.NotableItems),
		"sample":        s.Sample,
	}
}

// SynopsisFromMap decodes a persisted synopsis map. Unknown keys are
// ignored; missing fields default to zero values. Empty slices are
// preserved as empty (not nil) so the round-trip with ToMap stays stable.
func SynopsisFromMap(data map[string]any) ToolResultSynopsis {
	toStrings := func(v any) []string {
		arr, ok := v.([]any)
		if !ok {
			return []string{}
		}
		if len(arr) == 0 {
			return []string{}
		}
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			out = append(out, fmt.Sprint(item))
		}
		return out
	}
	return ToolResultSynopsis{
		Kind:         ToolResultKind(asString(data["kind"], "unknown")),
		Title:        asString(data["title"], ""),
		Summary:      toStrings(data["summary"]),
		Structure:    toStrings(data["structure"]),
		NotableItems: toStrings(data["notable_items"]),
		Sample:       asString(data["sample"], ""),
	}
}

var (
	codePattern1 = regexp.MustCompile(`(?m)^\s*(from\s+\S+\s+import|import\s+\S+)`)
	codePattern2 = regexp.MustCompile(`(?m)^\s*(class|def|async\s+def|function|export\s+function|const|let|var)\s+\w+`)
	codePattern3 = regexp.MustCompile(`(?m)^\s*(package|use|pub\s+fn|fn|public\s+class)\s+\w+`)
	codePatterns = []*regexp.Regexp{codePattern1, codePattern2, codePattern3}

	importRE = regexp.MustCompile(`^(?:from\s+(\S+)\s+import|import\s+(\S+))`)
	symbolRE = regexp.MustCompile(`^(class|def|async\s+def|function|export\s+function|pub\s+fn|fn)\s+([A-Za-z_]\w*)`)

	mdHeaderRE       = regexp.MustCompile(`^#{1,6}\s+`)
	allCapsHeaderRE  = regexp.MustCompile(`^[A-Z0-9][A-Z0-9\s:_-]{6,}$`)
	yamlKeyRE        = regexp.MustCompile(`^[A-Za-z0-9_.-]+:\s*(?:#.*)?$`)
	whitespaceRunRE  = regexp.MustCompile(`\s+`)
	safeIDStripRE    = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	unsafeIDCharRE   = regexp.MustCompile(`[^A-Za-z0-9_.-]`)
)

// GenerateSynopsis detects the shape of content and returns a synopsis.
// previewChars bounds samples for binary-like output; pass 0 to suppress.
// toolName and mimeType are hints — mime type is authoritative when set.
func GenerateSynopsis(content string, previewChars int, toolName, mimeType string) ToolResultSynopsis {
	if content == "" {
		return ToolResultSynopsis{
			Kind:    KindUnknown,
			Title:   "Empty output",
			Summary: []string{"Output is empty."},
		}
	}
	if looksBinary(content) {
		return ToolResultSynopsis{
			Kind:    KindUnknown,
			Title:   "Binary-like output",
			Summary: []string{fmt.Sprintf("Contains %d characters and non-text control bytes.", len(content))},
			Sample:  headTailSample(content, previewChars),
		}
	}

	stripped := strings.TrimSpace(content)
	lowerMime := strings.ToLower(mimeType)
	if strings.Contains(lowerMime, "json") || strings.HasPrefix(stripped, "{") || strings.HasPrefix(stripped, "[") {
		if value, trailing, ok := tryJSON(stripped); ok {
			return summarizeJSON(value, trailing)
		}
		if strings.Contains(lowerMime, "json") {
			return ToolResultSynopsis{
				Kind:    KindUnknown,
				Title:   "Unparsed JSON-like output",
				Summary: []string{"JSON-like output failed to parse."},
				Sample:  headTailSample(content, previewChars),
			}
		}
	}
	if strings.Contains(lowerMime, "xml") || strings.HasPrefix(stripped, "<") {
		if root, ok := tryXML(content); ok {
			return summarizeXML(root)
		}
		if strings.Contains(lowerMime, "xml") {
			return ToolResultSynopsis{
				Kind:    KindUnknown,
				Title:   "Unparsed XML-like output",
				Summary: []string{"XML-like output failed to parse."},
				Sample:  headTailSample(content, previewChars),
			}
		}
	}
	if strings.Contains(content, "\t") {
		if table := tryTable(content, "\t", KindTSV); table != nil {
			return *table
		}
	}
	if strings.Contains(content, ",") {
		if table := tryTable(content, ",", KindCSV); table != nil {
			return *table
		}
	}
	if yamlValue := tryYAML(content); yamlValue != nil {
		return summarizeYAML(yamlValue)
	}
	if anyCodePatternMatched(content) {
		return summarizeCode(content)
	}
	return summarizeText(content)
}

// RenderToolResultStub renders the deterministic stub that replaces the
// raw tool output in session messages. The stub points callers at the
// persisted raw bytes via ref.
func RenderToolResultStub(syn ToolResultSynopsis, ref, toolName, sha256, reason string, originalChars, previewChars int) string {
	header := []string{
		"[OpenViking tool result externalized]",
		fmt.Sprintf("tool_name: %s", orDefault(toolName, "tool")),
		fmt.Sprintf("kind: %s", syn.Kind),
		fmt.Sprintf("original_chars: %d", originalChars),
		fmt.Sprintf("preview_chars: %d", maxInt(previewChars, 0)),
	}
	if ref != "" {
		header = append(header, fmt.Sprintf("ref: %s", ref))
	}
	if sha256 != "" {
		header = append(header, fmt.Sprintf("sha256: %s", safeSha256Prefix(sha256, 16)))
	}
	if reason != "" {
		header = append(header, fmt.Sprintf("reason: %s", reason))
	}

	body := []string{"Synopsis:"}
	for _, line := range syn.Summary {
		body = append(body, "- "+line)
	}
	if len(syn.Structure) > 0 {
		body = append(body, "", "Structure:")
		for _, line := range syn.Structure {
			body = append(body, "- "+line)
		}
	}
	if len(syn.NotableItems) > 0 {
		body = append(body, "", "Notable items:")
		for _, line := range syn.NotableItems {
			body = append(body, "- "+line)
		}
	}
	if syn.Sample != "" {
		body = append(body, "", "Sample:", syn.Sample)
	}
	if ref != "" {
		body = append(body, "", "Explore:",
			fmt.Sprintf("- Use openviking_tool_result_search with ref=%s to find relevant raw snippets.", ref),
			fmt.Sprintf("- Use openviking_tool_result_read with ref=%s and offset/limit to inspect raw output.", ref),
			"- Use openviking_tool_result_list to discover other externalized outputs in this session.",
		)
	}
	return strings.Join(header, "\n") + "\n\n" + strings.Join(body, "\n")
}

// --- helpers ---

func clip(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	cutAt := limit - 3
	if cutAt < 0 {
		cutAt = 0
	}
	return value[:cutAt] + "..."
}

func normalizeTextForLine(text string, maxLen int) string {
	compact := whitespaceRunRE.ReplaceAllString(text, " ")
	compact = strings.TrimSpace(compact)
	return clip(compact, maxLen)
}

func headTailSample(content string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(content) <= limit {
		return content
	}
	half := limit / 2
	if half < 1 {
		half = 1
	}
	return "--- BEGIN SAMPLE HEAD ---\n" +
		content[:half] + "\n" +
		"--- END SAMPLE HEAD ---\n\n" +
		"--- BEGIN SAMPLE TAIL ---\n" +
		content[len(content)-half:] + "\n" +
		"--- END SAMPLE TAIL ---"
}

func looksBinary(content string) bool {
	if content == "" {
		return false
	}
	if strings.ContainsRune(content, 0) {
		return true
	}
	sample := content
	if len(sample) > 1000 {
		sample = sample[:1000]
	}
	control := 0
	for _, r := range sample {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			control++
		}
	}
	threshold := float64(len(sample)) * 0.05
	if threshold < 1 {
		threshold = 1
	}
	return float64(control) > threshold
}

func typeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case bool:
		return "boolean"
	case nil:
		return "null"
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "number"
	case string:
		return "string"
	default:
		return "string"
	}
}

func jsonScalarExamples(value any, prefix string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	examples := []string{}
	var rec func(v any, p string)
	rec = func(v any, p string) {
		if len(examples) >= limit {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				childPrefix := p
				if p != "" {
					childPrefix = p + "." + k
				} else {
					childPrefix = k
				}
				rec(child, childPrefix)
				if len(examples) >= limit {
					return
				}
			}
		case []any:
			if len(t) > 0 {
				rec(t[0], p+"[0]")
			}
		default:
			examples = append(examples, fmt.Sprintf("%s: %s", p, clip(fmt.Sprintf("%v", v), 80)))
		}
	}
	rec(value, prefix)
	if len(examples) > limit {
		examples = examples[:limit]
	}
	return examples
}

func jsonShape(value any, depth int) string {
	if depth >= jsonMaxDepth {
		return "..."
	}
	switch t := value.(type) {
	case []any:
		samples := make([]string, 0, jsonArraySampleLimit)
		for i := 0; i < len(t) && i < jsonArraySampleLimit; i++ {
			samples = append(samples, jsonShape(t[i], depth+1))
		}
		sampleText := ""
		if len(samples) > 0 {
			sampleText = fmt.Sprintf(", sample=[%s]", strings.Join(samples, ", "))
		}
		return fmt.Sprintf("array(len=%d%s)", len(t), sampleText)
	case map[string]any:
		keys := make([]string, 0, jsonObjectKeyLimit)
		i := 0
		for k := range t {
			if i >= jsonObjectKeyLimit {
				break
			}
			keys = append(keys, k)
			i++
		}
		keyText := ""
		if len(keys) > 0 {
			keyText = ": " + strings.Join(keys, ", ")
		}
		return fmt.Sprintf("object(keys=%d%s)", len(t), keyText)
	default:
		return typeName(value)
	}
}

func summarizeJSON(value any, trailingChars int) ToolResultSynopsis {
	summary := []string{fmt.Sprintf("JSON %s.", typeName(value))}
	structure := []string{}
	notable := []string{}

	switch t := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		preview := make([]string, 0, jsonObjectKeyLimit)
		for i, k := range keys {
			if i >= jsonObjectKeyLimit {
				break
			}
			preview = append(preview, k)
		}
		summary = append(summary, fmt.Sprintf("top-level keys: %s", strings.Join(preview, ", ")))
		structure = append(structure, fmt.Sprintf("shape: %s", jsonShape(value, 0)))
		i := 0
		for _, k := range keys {
			if i >= jsonObjectKeyLimit {
				break
			}
			child := t[k]
			switch c := child.(type) {
			case []any:
				structure = append(structure, fmt.Sprintf("%s: array length: %d", k, len(c)))
			case map[string]any:
				childKeys := make([]string, 0, jsonObjectKeyLimit)
				j := 0
				for ck := range c {
					if j >= jsonObjectKeyLimit {
						break
					}
					childKeys = append(childKeys, ck)
					j++
				}
				structure = append(structure, fmt.Sprintf("%s: object keys: %s", k, strings.Join(childKeys, ", ")))
			default:
				structure = append(structure, fmt.Sprintf("%s: %s", k, typeName(child)))
			}
			i++
		}
	case []any:
		summary = append(summary, fmt.Sprintf("array length: %d", len(t)))
		structure = append(structure, fmt.Sprintf("shape: %s", jsonShape(value, 0)))
		if len(t) > 0 {
			structure = append(structure, fmt.Sprintf("first item type: %s", typeName(t[0])))
		}
	}
	notable = append(notable, jsonScalarExamples(value, "", 5)...)
	if trailingChars > 0 {
		notable = append(notable, fmt.Sprintf("trailing_chars_after_first_json_value: %d", trailingChars))
	}
	return ToolResultSynopsis{
		Kind:         KindJSON,
		Title:        "JSON output",
		Summary:      summary,
		Structure:    structure,
		NotableItems: notable,
	}
}

func summarizeYAML(value any) ToolResultSynopsis {
	summary := []string{fmt.Sprintf("YAML %s.", typeName(value))}
	structure := []string{}
	switch t := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		preview := make([]string, 0, yamlKeyLimit)
		for i, k := range keys {
			if i >= yamlKeyLimit {
				break
			}
			preview = append(preview, k)
		}
		summary = append(summary, fmt.Sprintf("top-level keys: %s", strings.Join(preview, ", ")))
		i := 0
		for _, k := range keys {
			if i >= yamlKeyLimit {
				break
			}
			structure = append(structure, fmt.Sprintf("%s: %s", k, typeName(t[k])))
			i++
		}
	case []any:
		summary = append(summary, fmt.Sprintf("array length: %d", len(t)))
	}
	return ToolResultSynopsis{
		Kind:      KindYAML,
		Title:     "YAML output",
		Summary:   summary,
		Structure: structure,
	}
}

func summarizeXML(root *xmlNode) ToolResultSynopsis {
	structure := []string{
		fmt.Sprintf("root: %s", root.tag),
		fmt.Sprintf("attributes: %d", len(root.attrs)),
	}
	for tag, count := range root.childCounts(xmlChildTagLimit) {
		structure = append(structure, fmt.Sprintf("%s: %d", tag, count))
	}
	return ToolResultSynopsis{
		Kind:      KindXML,
		Title:     "XML output",
		Summary:   []string{fmt.Sprintf("XML document with root: %s.", root.tag)},
		Structure: structure,
	}
}

func tryTable(content, delimiter string, kind ToolResultKind) *ToolResultSynopsis {
	reader := csv.NewReader(strings.NewReader(content))
	reader.Comma = rune(delimiter[0])
	reader.FieldsPerRecord = -1
	rows, err := reader.ReadAll()
	if err != nil {
		return nil
	}
	// Drop empty rows.
	cleaned := rows[:0]
	for _, row := range rows {
		empty := true
		for _, cell := range row {
			if strings.TrimSpace(cell) != "" {
				empty = false
				break
			}
		}
		if !empty {
			cleaned = append(cleaned, row)
		}
	}
	if len(cleaned) < 2 || len(cleaned[0]) < 2 {
		return nil
	}
	colCount := len(cleaned[0])
	for i := 1; i < len(cleaned) && i < 10; i++ {
		if len(cleaned[i]) != colCount {
			return nil
		}
	}
	columns := make([]string, 0, colCount)
	for idx, cell := range cleaned[0] {
		name := strings.TrimSpace(cell)
		if name == "" {
			name = fmt.Sprintf("column_%d", idx+1)
		}
		columns = append(columns, name)
	}
	dataRows := len(cleaned) - 1
	firstData := "(no data rows)"
	if len(cleaned) > 1 {
		firstData = normalizeTextForLine(strings.Join(cleaned[1], delimiter), tableFirstRowSample)
	}
	return &ToolResultSynopsis{
		Kind:      kind,
		Title:     fmt.Sprintf("%s table output", strings.ToUpper(string(kind))),
		Summary:   []string{fmt.Sprintf("%s table with rows: %d and columns: %d.", strings.ToUpper(string(kind)), dataRows, colCount)},
		Structure: []string{
			fmt.Sprintf("columns: %s", strings.Join(columns, ", ")),
			fmt.Sprintf("rows: %d", dataRows),
			fmt.Sprintf("first row sample: %s", firstData),
		},
	}
}

func tryJSON(stripped string) (value any, trailing int, ok bool) {
	dec := json.NewDecoder(strings.NewReader(stripped))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, 0, false
	}
	rest := strings.TrimSpace(stripped[dec.InputOffset():])
	return v, len(rest), true
}

func tryXML(content string) (*xmlNode, bool) {
	root, err := parseXML(content)
	if err != nil {
		return nil, false
	}
	return root, true
}

func tryYAML(content string) any {
	if !looksYAML(content) {
		return nil
	}
	var v any
	if err := yaml.Unmarshal([]byte(content), &v); err != nil {
		return nil
	}
	switch t := v.(type) {
	case map[string]any, []any:
		return t
	}
	return nil
}

func looksYAML(content string) bool {
	stripped := strings.TrimLeft(content, " \t\r\n")
	if strings.HasPrefix(stripped, "---") {
		return true
	}
	if anyCodePatternMatched(content) {
		return false
	}
	lines := []string{}
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	for i := 0; i < len(lines)-1; i++ {
		if yamlKeyRE.MatchString(lines[i]) {
			if strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t") {
				return true
			}
		}
	}
	return false
}

func anyCodePatternMatched(content string) bool {
	for _, p := range codePatterns {
		if p.MatchString(content) {
			return true
		}
	}
	return false
}

func summarizeCode(content string) ToolResultSynopsis {
	lines := strings.Split(content, "\n")
	imports := []string{}
	symbols := []string{}
	for _, line := range lines {
		stripped := strings.TrimSpace(line)
		if m := importRE.FindStringSubmatch(stripped); m != nil {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			imports = append(imports, normalizeTextForLine(name, codeImportLineChars))
			continue
		}
		if m := symbolRE.FindStringSubmatch(stripped); m != nil {
			symbols = append(symbols, normalizeTextForLine(m[1]+" "+m[2], codeSymbolLineChars))
		}
	}
	structure := []string{fmt.Sprintf("line_count: %d", len(lines))}
	if len(imports) > 0 {
		preview := imports
		if len(preview) > codeImportLimit {
			preview = preview[:codeImportLimit]
		}
		structure = append(structure, fmt.Sprintf("imports: %s", strings.Join(preview, ", ")))
	}
	if len(symbols) > codeSymbolLimit {
		symbols = symbols[:codeSymbolLimit]
	}
	return ToolResultSynopsis{
		Kind:         KindCode,
		Title:        "Code output",
		Summary:      []string{fmt.Sprintf("Code-like output with %d lines.", len(lines))},
		Structure:    structure,
		NotableItems: symbols,
	}
}

func summarizeText(content string) ToolResultSynopsis {
	lines := strings.Split(content, "\n")
	normalized := strings.TrimSpace(whitespaceRunRE.ReplaceAllString(content, " "))
	headers := []string{}
	seen := map[string]struct{}{}
	for _, line := range lines {
		stripped := strings.TrimSpace(line)
		if len(stripped) <= 1 {
			continue
		}
		if !mdHeaderRE.MatchString(stripped) && !allCapsHeaderRE.MatchString(stripped) {
			continue
		}
		header := normalizeTextForLine(stripped, 160)
		if _, dup := seen[header]; dup {
			continue
		}
		seen[header] = struct{}{}
		headers = append(headers, header)
		if len(headers) >= textHeaderLimit {
			break
		}
	}
	wordCount := 0
	if normalized != "" {
		wordCount = len(strings.Fields(normalized))
	}
	first := normalizeTextForLine(content[:minInt(len(content), textExcerptChars)], textExcerptChars)
	last := ""
	if len(content) > textExcerptChars {
		last = normalizeTextForLine(content[len(content)-textExcerptChars:], textExcerptChars)
	}
	headersText := "none detected"
	if len(headers) > 0 {
		headersText = strings.Join(headers, " | ")
	}
	summary := []string{
		"Text exploration summary:",
		fmt.Sprintf("Characters: %d.", len(content)),
		fmt.Sprintf("Words: %d.", wordCount),
		fmt.Sprintf("Lines: %d.", len(lines)),
		fmt.Sprintf("Detected section headers: %s.", headersText),
		fmt.Sprintf("Opening excerpt: %s.", orDefault(first, "(empty)")),
		fmt.Sprintf("Closing excerpt: %s.", orDefault(last, "(empty)")),
	}
	return ToolResultSynopsis{
		Kind:    KindText,
		Title:   "Text output",
		Summary: summary,
	}
}

// --- xml support ---

// xmlNode is a tiny wrapper around encoding/xml's start element that
// preserves child tag counts for the synopsis.
type xmlNode struct {
	tag    string
	attrs  map[string]string
	childs []*xmlNode
}

func (n *xmlNode) childCounts(limit int) map[string]int {
	counts := map[string]int{}
	for _, c := range n.childs {
		counts[c.tag]++
	}
	if limit <= 0 {
		return counts
	}
	// Keep top `limit` tags by count.
	type kv struct{ k string; v int }
	pairs := make([]kv, 0, len(counts))
	for k, v := range counts {
		pairs = append(pairs, kv{k, v})
	}
	// Simple sort by v desc; stable enough for our needs.
	for i := 0; i < len(pairs); i++ {
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].v > pairs[i].v {
				pairs[i], pairs[j] = pairs[j], pairs[i]
			}
		}
	}
	out := map[string]int{}
	for i, p := range pairs {
		if i >= limit {
			break
		}
		out[p.k] = p.v
	}
	return out
}

func parseXML(content string) (*xmlNode, error) {
	dec := xml.NewDecoder(strings.NewReader(content))
	var root *xmlNode
	var stack []*xmlNode
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			attrs := map[string]string{}
			for _, a := range t.Attr {
				attrs[a.Name.Local] = a.Value
			}
			node := &xmlNode{tag: t.Name.Local, attrs: attrs}
			if root == nil {
				root = node
			} else if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.childs = append(parent.childs, node)
			}
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("no root element")
	}
	return root, nil
}

// --- misc ---

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func asString(v any, def string) string {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func safeSha256Prefix(sha string, n int) string {
	if sha == "" {
		return ""
	}
	// utf8.RuneCountInStr lets us cut by rune, not byte, for safety.
	count := utf8.RuneCountInString(sha)
	if count <= n {
		return sha
	}
	runes := []rune(sha)
	return string(runes[:n])
}

// SafeID replaces any run of disallowed chars with underscore and trims
// leading/trailing dots/dashes/underscores. Returns fallback when empty.
func SafeID(value, fallback string) string {
	safe := safeIDStripRE.ReplaceAllString(value, "_")
	safe = strings.Trim(safe, "._-")
	if safe == "" {
		return fallback
	}
	if len(safe) > 96 {
		safe = safe[:96]
	}
	return safe
}

// BuildToolResultID mirrors openviking.session.tool_result_store.build_tool_result_id.
func BuildToolResultID(toolID, sha256 string) string {
	if toolID != "" {
		return fmt.Sprintf("tr_%s_%s", SafeID(toolID, "tool"), safeSha256Prefix(sha256, 16))
	}
	return fmt.Sprintf("tr_%s", safeSha256Prefix(sha256, 24))
}

// ValidateToolResultID returns true when id is a safe persisted tool result
// identifier (no path separators, no disallowed chars).
func ValidateToolResultID(id string) bool {
	if id == "" {
		return false
	}
	if strings.Contains(id, "/") {
		return false
	}
	return !unsafeIDCharRE.MatchString(id)
}
