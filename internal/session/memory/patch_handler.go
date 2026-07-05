// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"errors"
	"regexp"
	"strings"
)

// PatchParseError is returned when a patch cannot be applied. It
// mirrors openviking.session.memory.merge_op.patch_handler.PatchParseError.
type PatchParseError struct{ msg string }

// Error implements the error interface.
func (e *PatchParseError) Error() string {
	if e == nil {
		return "patch parse error"
	}
	return "patch parse error: " + e.msg
}

// NewPatchParseError returns a PatchParseError with the given message.
func NewPatchParseError(msg string) error { return &PatchParseError{msg: msg} }

// DiffResult is the outcome of applying a diff. It mirrors
// openviking.session.memory.merge_op.patch_handler.DiffResult.
type DiffResult struct {
	Success   bool
	Content   string
	Error     string
	FailParts []map[string]any
}

// MultiSearchReplaceDiffStrategy is the RooCode-inspired multi-block
// SEARCH/REPLACE strategy. It mirrors
// openviking.session.memory.merge_op.patch_handler.MultiSearchReplaceDiffStrategy.
type MultiSearchReplaceDiffStrategy struct {
	FuzzyThreshold float64
	BufferLines    int
}

// NewMultiSearchReplaceDiffStrategy returns a strategy with the given
// fuzzy threshold and buffer-line count.
func NewMultiSearchReplaceDiffStrategy(threshold float64, bufferLines int) *MultiSearchReplaceDiffStrategy {
	if threshold <= 0 {
		threshold = 1.0
	}
	if bufferLines <= 0 {
		bufferLines = 40
	}
	return &MultiSearchReplaceDiffStrategy{FuzzyThreshold: threshold, BufferLines: bufferLines}
}

// marker sequencing state (from RooCode).
type markerState int

const (
	markerStart markerState = iota
	markerAfterSearch
	markerAfterSeparator
)

var (
	searchPatternRE   = regexp.MustCompile(`^<<<<<<< SEARCH>?$`)
	separatorMarker  = "======="
	replaceMarker    = ">>>>>>> REPLACE"
	searchPrefix     = "<<<<<<<"
	replacePrefix    = ">>>>>>>"

	lineNumberRE               = regexp.MustCompile(`^(\d+)\t`)
	lineNumberLeadingSpaceRE   = regexp.MustCompile(`^\s*(\d+)\t`)
	lineSplitRE                = regexp.MustCompile(`\r?\n`)
	indentRE                   = regexp.MustCompile(`^[\t ]*`)
	escapedMarkerSearchRE      = regexp.MustCompile(`^\\<<<<<<<`)
	escapedMarkerSeparatorRE   = regexp.MustCompile(`^\\=======`)
	escapedMarkerReplaceRE     = regexp.MustCompile(`^\\>>>>>>>`)
	memoryFieldsCommentRE      = regexp.MustCompile(`\n\n<!--\s*MEMORY_FIELDS\s*\n(.*?)\n-->`)
	memoryFieldsCommentEndRE   = regexp.MustCompile(`<!--\s*MEMORY_FIELDS\s*\n(.*?)\n-->$`)
	cjkRE                      = regexp.MustCompile(`[㐀-䶿一-鿿豈-﫿]`)
	asciiWordCharRE            = regexp.MustCompile(`[A-Za-z0-9_]`)
	relativeLinkRE             = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	uriLanguageNoiseRE         = regexp.MustCompile(`\b(?:viking|https?)://[^\s<>\]\)"']+`)
	codeBlockRE                = regexp.MustCompile("```.*?```")
	inlineCodeRE               = regexp.MustCompile("`[^`\n]+`")
)

// SplitContentLines splits content on \r?\n. Empty input yields [].
func SplitContentLines(content string) []string {
	if content == "" {
		return nil
	}
	return lineSplitRE.Split(content, -1)
}

// AddLineNumbers prefixes each line with "{n}\t".
func AddLineNumbers(content string, startLine int) string {
	if content == "" {
		return ""
	}
	if startLine <= 0 {
		startLine = 1
	}
	lines := SplitContentLines(content)
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		out = append(out, formatLineNumber(startLine+i)+ "\t" + line)
	}
	return strings.Join(out, "\n")
}

// SliceContentLines returns the slice of content [offset, offset+limit).
// limit<0 means "to end".
func SliceContentLines(content string, offset, limit int) string {
	lines := SplitContentLines(content)
	if offset >= len(lines) {
		return ""
	}
	end := len(lines)
	if limit >= 0 {
		end = offset + limit
		if end > len(lines) {
			end = len(lines)
		}
	}
	return strings.Join(lines[offset:end], "\n")
}

// LineCount returns the number of lines in content.
func LineCount(content string) int { return len(SplitContentLines(content)) }

// ExtractStartLineNumber reads a leading "{n}\t" line number from
// content. Returns -1 when absent.
func ExtractStartLineNumber(content string) int {
	first := content
	if idx := strings.Index(content, "\n"); idx >= 0 {
		first = content[:idx]
	}
	m := lineNumberLeadingSpaceRE.FindStringSubmatch(first)
	if m == nil {
		return -1
	}
	i, ok := asInt(m[1])
	if !ok {
		return -1
	}
	return i
}

// StripLineNumbers removes leading "{n}\t" prefixes from each line.
// When aggressive is true, leading whitespace before the prefix is also
// stripped.
func StripLineNumbers(content string, aggressive bool) bool {
	pattern := lineNumberRE
	if aggressive {
		pattern = lineNumberLeadingSpaceRE
	}
	lines := SplitContentLines(content)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, pattern.ReplaceAllString(line, ""))
	}
	return strings.Join(out, "\n") != "" || content == ""
}

// stripLineNumbersContent returns the content with line-number prefixes
// stripped. (Companion to StripLineNumbers that returns the string
// rather than a bool.)
func stripLineNumbersContent(content string, aggressive bool) string {
	pattern := lineNumberRE
	if aggressive {
		pattern = lineNumberLeadingSpaceRE
	}
	lines := SplitContentLines(content)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, pattern.ReplaceAllString(line, ""))
	}
	return strings.Join(out, "\n")
}

// EveryLineHasLineNumbers reports whether every line in content has a
// leading "{n}\t" prefix.
func EveryLineHasLineNumbers(content string) bool {
	lines := SplitContentLines(content)
	if len(lines) == 0 {
		return false
	}
	for _, line := range lines {
		if !lineNumberRE.MatchString(line) {
			return false
		}
	}
	return true
}

// LevenshteinDistance computes the edit distance between s1 and s2.
func LevenshteinDistance(s1, s2 string) int {
	if len(s1) < len(s2) {
		return LevenshteinDistance(s2, s1)
	}
	if len(s2) == 0 {
		return len(s1)
	}
	previous := make([]int, len(s2)+1)
	for i := range previous {
		previous[i] = i
	}
	for i := 0; i < len(s1); i++ {
		current := make([]int, len(s2)+1)
		current[0] = i + 1
		for j := 0; j < len(s2); j++ {
			insertions := previous[j+1] + 1
			deletions := current[j] + 1
			subst := previous[j]
			if s1[i] != s2[j] {
				subst++
			}
			current[j+1] = minInt3(insertions, deletions, subst)
		}
		previous = current
	}
	return previous[len(previous)-1]
}

// NormalizeString normalises smart quotes and zero-width characters.
func NormalizeString(text string) string {
	replacements := []struct{ R rune; With string }{
		{0x2018, "'"},
		{0x2019, "'"},
		{0x201C, `"`},
		{0x201D, `"`},
		{0x00A0, " "},
		{0x200B, ""},
		{0x200C, ""},
		{0x200D, ""},
		{0x200E, ""},
		{0x200F, ""},
		{0xFEFF, ""},
	}
	for _, r := range replacements {
		text = strings.ReplaceAll(text, string(r.R), r.With)
	}
	return text
}

// GetSimilarity returns the similarity ratio (0..1) between two strings.
func GetSimilarity(original, search string) float64 {
	if search == "" {
		return 0
	}
	no := NormalizeString(original)
	ns := NormalizeString(search)
	if no == ns {
		return 1
	}
	dist := LevenshteinDistance(no, ns)
	maxLen := len(no)
	if len(ns) > maxLen {
		maxLen = len(ns)
	}
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(dist)/float64(maxLen)
}

// fuzzySearchResult is the result of a fuzzy search.
type fuzzySearchResult struct {
	BestScore    float64
	BestMatchIdx int
	BestMatch    string
}

// fuzzySearch performs a "middle-out" search to find the slice most
// similar to searchChunk. For single-line search it also checks for
// substring matches.
func fuzzySearch(lines []string, searchChunk string, startIdx, endIdx int) fuzzySearchResult {
	out := fuzzySearchResult{BestMatchIdx: -1}
	searchLines := strings.Split(searchChunk, "\n")
	searchLen := len(searchLines)
	midPoint := (startIdx + endIdx) / 2
	leftIdx := midPoint
	rightIdx := midPoint + 1

	isSingleLine := searchLen == 1
	var searchStr string
	if isSingleLine {
		searchStr = searchLines[0]
	}

	for leftIdx >= startIdx || rightIdx <= endIdx-searchLen {
		if leftIdx >= startIdx {
			if isSingleLine {
				line := lines[leftIdx]
				if strings.Contains(line, searchStr) {
					out.BestScore = 1
					out.BestMatchIdx = leftIdx
					out.BestMatch = line
					leftIdx--
					continue
				}
				score, content := findBestSubstringMatch(line, searchStr)
				if score > out.BestScore {
					out.BestScore = score
					out.BestMatchIdx = leftIdx
					out.BestMatch = content
				}
			} else {
				end := leftIdx + searchLen
				if end > len(lines) {
					end = len(lines)
				}
				chunk := strings.Join(lines[leftIdx:end], "\n")
				score := GetSimilarity(chunk, searchChunk)
				if score > out.BestScore {
					out.BestScore = score
					out.BestMatchIdx = leftIdx
					out.BestMatch = chunk
				}
			}
			leftIdx--
		}
		if rightIdx <= endIdx-searchLen {
			if isSingleLine {
				line := lines[rightIdx]
				if strings.Contains(line, searchStr) {
					out.BestScore = 1
					out.BestMatchIdx = rightIdx
					out.BestMatch = line
					rightIdx++
					continue
				}
				score, content := findBestSubstringMatch(line, searchStr)
				if score > out.BestScore {
					out.BestScore = score
					out.BestMatchIdx = rightIdx
					out.BestMatch = content
				}
			} else {
				end := rightIdx + searchLen
				if end > len(lines) {
					end = len(lines)
				}
				chunk := strings.Join(lines[rightIdx:end], "\n")
				score := GetSimilarity(chunk, searchChunk)
				if score > out.BestScore {
					out.BestScore = score
					out.BestMatchIdx = rightIdx
					out.BestMatch = chunk
				}
			}
			rightIdx++
		}
	}
	return out
}

// findBestSubstringMatch finds the best matching substring in a line.
func findBestSubstringMatch(line, searchStr string) (float64, string) {
	bestScore := 0.0
	bestContent := ""
	searchLen := len(searchStr)
	lineLen := len(line)

	if searchLen >= lineLen {
		return GetSimilarity(line, searchStr), line
	}

	positions := []int{0, lineLen - searchLen}
	if lineLen > searchLen*3 {
		positions = append(positions, lineLen/2-searchLen/2)
	}
	for _, i := range positions {
		if i < 0 || i > lineLen-searchLen {
			continue
		}
		sub := line[i : i+searchLen]
		score := GetSimilarity(sub, searchStr)
		if score > bestScore {
			bestScore = score
			bestContent = sub
		}
	}

	whole := GetSimilarity(line, searchStr)
	if whole > bestScore {
		bestScore = whole
		bestContent = line
	}
	return bestScore, bestContent
}

// unescapeMarkers unescapes escaped SEARCH/REPLACE markers.
func unescapeMarkers(content string) string {
	content = escapedMarkerSearchRE.ReplaceAllString(content, "<<<<<<<")
	content = escapedMarkerSeparatorRE.ReplaceAllString(content, "=======")
	content = escapedMarkerReplaceRE.ReplaceAllString(content, ">>>>>>>")
	content = strings.ReplaceAll(content, `\-------`, "-------")
	content = strings.ReplaceAll(content, `\:end_line:`, ":end_line:")
	content = strings.ReplaceAll(content, `\:start_line:`, ":start_line:")
	return content
}

// validateMarkerSequencing validates the marker sequencing in diff
// content. Returns a non-nil error describing the first violation.
func validateMarkerSequencing(diffContent string) error {
	state := markerStart
	lineNo := 0

	lines := strings.Split(diffContent, "\n")
	searchCount := 0
	sepCount := 0
	replaceCount := 0
	for _, l := range lines {
		stripped := strings.TrimSpace(l)
		if searchPatternRE.MatchString(stripped) {
			searchCount++
		}
		if stripped == separatorMarker {
			sepCount++
		}
		if stripped == replaceMarker {
			replaceCount++
		}
	}
	likelyBad := searchCount != replaceCount || sepCount < searchCount

	for _, line := range lines {
		lineNo++
		marker := strings.TrimSpace(line)

		if state == markerAfterSeparator {
			if strings.HasPrefix(marker, ":start_line:") && !strings.HasPrefix(strings.TrimSpace(line), `\:start_line:`) {
				return errors.New("invalid :start_line: marker in REPLACE section")
			}
			if strings.HasPrefix(marker, ":end_line:") && !strings.HasPrefix(strings.TrimSpace(line), `\:end_line:`) {
				return errors.New("invalid :end_line: marker in REPLACE section")
			}
		}

		switch state {
		case markerStart:
			if marker == separatorMarker {
				if likelyBad {
					return errors.New("unexpected '=======' before SEARCH")
				}
				return errors.New("merge conflict marker '=======' found before SEARCH")
			}
			if marker == replaceMarker {
				return errors.New("unexpected '>>>>>>> REPLACE' before SEARCH")
			}
			if strings.HasPrefix(marker, replacePrefix) {
				return errors.New("merge conflict marker '" + marker + "' found before SEARCH")
			}
			if searchPatternRE.MatchString(marker) {
				state = markerAfterSearch
			} else if strings.HasPrefix(marker, searchPrefix) {
				return errors.New("merge conflict marker '" + marker + "' found before SEARCH")
			}
		case markerAfterSearch:
			if searchPatternRE.MatchString(marker) {
				return errors.New("unexpected '<<<<<<< SEARCH' after SEARCH")
			}
			if strings.HasPrefix(marker, searchPrefix) {
				return errors.New("merge conflict marker '" + marker + "' found after SEARCH")
			}
			if marker == replaceMarker {
				return errors.New("unexpected '>>>>>>> REPLACE' after SEARCH")
			}
			if strings.HasPrefix(marker, replacePrefix) {
				return errors.New("merge conflict marker '" + marker + "' found after SEARCH")
			}
			if marker == separatorMarker {
				state = markerAfterSeparator
			}
		case markerAfterSeparator:
			if searchPatternRE.MatchString(marker) {
				return errors.New("unexpected '<<<<<<< SEARCH' in REPLACE section")
			}
			if strings.HasPrefix(marker, searchPrefix) {
				return errors.New("merge conflict marker '" + marker + "' in REPLACE section")
			}
			if marker == separatorMarker {
				if likelyBad {
					return errors.New("unexpected '=======' in REPLACE section")
				}
				return errors.New("merge conflict marker '=======' found in REPLACE section")
			}
			if marker == replaceMarker {
				state = markerStart
			} else if strings.HasPrefix(marker, replacePrefix) {
				return errors.New("merge conflict marker '" + marker + "' in REPLACE section")
			}
		}
	}

	if state == markerStart {
		return nil
	}
	if state == markerAfterSearch {
		return errors.New("unexpected end of sequence: expected '=======' not found")
	}
	return errors.New("unexpected end of sequence: expected '>>>>>>> REPLACE' not found")
}

// diffBlock is one parsed SEARCH/REPLACE block.
type diffBlock struct {
	StartLine      int
	SearchContent  string
	ReplaceContent string
}

// parseDiffBlocks parses diff blocks from diff content. Supports both
// v1 (with :start_line: + -------) and v2 formats.
func parseDiffBlocks(diffContent string) []diffBlock {
	var matches []diffBlock
	blocks := strings.Split(diffContent, "<<<<<<< SEARCH")
	for _, block := range blocks[1:] {
		if !strings.Contains(block, "=======") || !strings.Contains(block, ">>>>>>> REPLACE") {
			continue
		}
		sepParts := strings.SplitN(block, "=======", 2)
		if len(sepParts) < 2 {
			continue
		}
		beforeSep := sepParts[0]
		afterSep := sepParts[1]

		var header, searchContent string
		dashParts := strings.SplitN(beforeSep, "-------", 2)
		if len(dashParts) >= 2 {
			header = dashParts[0]
			searchContent = strings.Trim(dashParts[1], "\n")
		} else {
			header = ""
			searchContent = strings.Trim(beforeSep, "\n")
			lines := strings.Split(searchContent, "\n")
			if len(lines) > 0 && strings.HasPrefix(lines[0], ":start_line:") {
				header = lines[0]
				searchContent = strings.Join(lines[1:], "\n")
			}
		}

		replaceContent := strings.SplitN(afterSep, ">>>>>>> REPLACE", 2)[0]
		replaceContent = strings.Trim(replaceContent, "\n")

		startLine := 0
		for _, line := range strings.Split(header, "\n") {
			if strings.HasPrefix(line, ":start_line:") {
				parts := strings.SplitN(line, ":", 3)
				if len(parts) >= 3 {
					if i, ok := asInt(strings.TrimSpace(parts[2])); ok {
						startLine = i
					}
				}
			}
		}

		matches = append(matches, diffBlock{
			StartLine:      startLine,
			SearchContent:  searchContent,
			ReplaceContent: replaceContent,
		})
	}
	return matches
}

// ApplyDiff applies a multi-search-replace diff to original content.
func (s *MultiSearchReplaceDiffStrategy) ApplyDiff(originalContent, diffContent string) DiffResult {
	if err := validateMarkerSequencing(diffContent); err != nil {
		return DiffResult{Success: false, Error: err.Error()}
	}

	matches := parseDiffBlocks(diffContent)
	if len(matches) == 0 {
		return DiffResult{Success: true, Content: originalContent}
	}

	// Phase 1: try simple substring replacement.
	result := originalContent
	allApplied := true
	var processed []diffBlock
	for _, m := range matches {
		search := unescapeMarkers(m.SearchContent)
		replace := unescapeMarkers(m.ReplaceContent)
		if search == replace {
			continue
		}
		hasLineNumbers := (EveryLineHasLineNumbers(search) && EveryLineHasLineNumbers(replace)) ||
			(EveryLineHasLineNumbers(search) && strings.TrimSpace(replace) == "")
		if hasLineNumbers {
			search = stripLineNumbersContent(search, false)
			replace = stripLineNumbersContent(replace, false)
		}
		if search == "" {
			allApplied = false
			break
		}
		if !strings.Contains(result, search) {
			allApplied = false
			break
		}
		processed = append(processed, diffBlock{SearchContent: search, ReplaceContent: replace})
	}
	if allApplied && len(processed) > 0 {
		for _, m := range processed {
			result = strings.ReplaceAll(result, m.SearchContent, m.ReplaceContent)
		}
		return DiffResult{Success: true, Content: result}
	}

	// Phase 2: line-based approach for complex cases.
	lineEnding := "\n"
	if strings.Contains(originalContent, "\r\n") {
		lineEnding = "\r\n"
	}
	resultLines := strings.Split(originalContent, "\n")
	if strings.HasSuffix(originalContent, "\r\n") {
		// Strip trailing empty entry from Split.
		if len(resultLines) > 0 && resultLines[len(resultLines)-1] == "" {
			resultLines = resultLines[:len(resultLines)-1]
		}
	}
	var diffResults []map[string]any
	appliedCount := 0

	replacements := make([]diffBlock, len(matches))
	copy(replacements, matches)
	// Already sorted by StartLine in Python; here we re-sort explicitly.
	sortDiffBlocksByStart(replacements)

	for _, r := range replacements {
		search := unescapeMarkers(r.SearchContent)
		replace := unescapeMarkers(r.ReplaceContent)
		startLine := r.StartLine

		hasAllLineNumbers := (EveryLineHasLineNumbers(search) && EveryLineHasLineNumbers(replace)) ||
			(EveryLineHasLineNumbers(search) && strings.TrimSpace(replace) == "")

		if hasAllLineNumbers && startLine == 0 {
			if inferred := ExtractStartLineNumber(search); inferred > 0 {
				startLine = inferred
			}
		}
		if hasAllLineNumbers {
			search = stripLineNumbersContent(search, false)
			replace = stripLineNumbersContent(replace, false)
		}

		if search == replace {
			diffResults = append(diffResults, map[string]any{
				"success": true,
				"message": "Search and replace content are identical - no changes needed",
			})
			continue
		}

		var searchLines, replaceLines []string
		if search != "" {
			searchLines = strings.Split(search, "\n")
		}
		if replace != "" {
			replaceLines = strings.Split(replace, "\n")
		}

		if len(searchLines) == 0 {
			diffResults = append(diffResults, map[string]any{
				"success": false,
				"error":   "Empty search content is not allowed",
			})
			continue
		}

		matchIdx := -1
		bestScore := 0.0
		searchChunk := strings.Join(searchLines, "\n")
		searchStartIdx := 0
		searchEndIdx := len(resultLines)

		if startLine != 0 {
			exactStart := startLine - 1
			searchLen := len(searchLines)
			exactEnd := exactStart + searchLen - 1
			if exactEnd >= len(resultLines) {
				exactEnd = len(resultLines) - 1
			}
			if exactStart >= 0 && exactStart <= exactEnd {
				originalChunk := strings.Join(resultLines[exactStart:exactEnd+1], "\n")
				similarity := GetSimilarity(originalChunk, searchChunk)
				if similarity >= s.FuzzyThreshold {
					matchIdx = exactStart
					bestScore = similarity
				} else {
					searchStartIdx = maxInt(0, startLine-(s.BufferLines+1))
					searchEndIdx = minInt(len(resultLines), startLine+len(searchLines)+s.BufferLines)
				}
			}
		}

		if matchIdx == -1 {
			fr := fuzzySearch(resultLines, searchChunk, searchStartIdx, searchEndIdx)
			matchIdx = fr.BestMatchIdx
			bestScore = fr.BestScore
		}

		if matchIdx == -1 || bestScore < s.FuzzyThreshold {
			aggressiveSearch := stripLineNumbersContent(search, true)
			aggressiveReplace := stripLineNumbersContent(replace, true)
			var aggressiveSearchLines []string
			if aggressiveSearch != "" {
				aggressiveSearchLines = strings.Split(aggressiveSearch, "\n")
			}
			aggressiveSearchChunk := strings.Join(aggressiveSearchLines, "\n")
			fr := fuzzySearch(resultLines, aggressiveSearchChunk, searchStartIdx, searchEndIdx)
			if fr.BestMatchIdx != -1 && fr.BestScore >= s.FuzzyThreshold {
				matchIdx = fr.BestMatchIdx
				bestScore = fr.BestScore
					search = aggressiveSearch
				replace = aggressiveReplace
				if aggressiveSearch != "" {
					searchLines = strings.Split(aggressiveSearch, "\n")
				} else {
					searchLines = nil
				}
				if aggressiveReplace != "" {
					replaceLines = strings.Split(aggressiveReplace, "\n")
				} else {
					replaceLines = nil
				}
			} else {
				diffResults = append(diffResults, map[string]any{
					"success": false,
					"error":   "No sufficiently similar match found",
				})
				continue
			}
		}

		end := matchIdx + len(searchLines)
		if end > len(resultLines) {
			end = len(resultLines)
		}
		matchedLines := resultLines[matchIdx:end]

		originalIndents := extractIndents(matchedLines)
		searchIndents := extractIndents(searchLines)

		indentedReplaceLines := make([]string, 0, len(replaceLines))
		for i, line := range replaceLines {
			var matchedIndent, searchIndent string
			if i < len(originalIndents) {
				matchedIndent = originalIndents[i]
			} else if len(originalIndents) > 0 {
				matchedIndent = originalIndents[0]
			}
			if i < len(searchIndents) {
				searchIndent = searchIndents[i]
			} else if len(searchIndents) > 0 {
				searchIndent = searchIndents[0]
			}
			currentReplaceIndent := ""
			if m := indentRE.FindString(line); m != "" {
				currentReplaceIndent = m
			}
			relativeLevel := len(currentReplaceIndent) - len(searchIndent)
			var finalIndent string
			if relativeLevel >= 0 {
				finalIndent = matchedIndent + currentReplaceIndent[len(searchIndent):]
			} else {
				k := len(matchedIndent) + relativeLevel
				if k < 0 {
					k = 0
				}
				finalIndent = matchedIndent[:k]
			}
			if strings.TrimSpace(line) == "" {
				indentedReplaceLines = append(indentedReplaceLines, matchedIndent)
			} else {
				content := strings.TrimLeft(line, " \t")
				indentedReplaceLines = append(indentedReplaceLines, finalIndent+content)
			}
		}

		before := resultLines[:matchIdx]
		after := resultLines[end:]
		resultLines = append(append(before, indentedReplaceLines...), after...)
		appliedCount++
	}

	finalContent := strings.Join(resultLines, lineEnding)

	hasFailures := false
	for _, r := range diffResults {
		success, _ := r["success"].(bool)
		if !success {
			hasFailures = true
			break
		}
	}

	if appliedCount == 0 && hasFailures {
		return DiffResult{Success: false, FailParts: diffResults}
	}

	if appliedCount == 0 && !hasFailures {
		return DiffResult{Success: true, Content: originalContent, FailParts: diffResults}
	}

	return DiffResult{Success: true, Content: finalContent, FailParts: diffResults}
}

// ApplyStrPatch applies a StrPatch (sequence of SEARCH/REPLACE blocks)
// to original content. It mirrors
// openviking.session.memory.merge_op.patch_handler.apply_str_patch.
func ApplyStrPatch(originalContent string, patch StrPatch) (string, error) {
	if len(patch.Blocks) == 0 {
		return originalContent, nil
	}

	// Phase 1: try simple substring replacement.
	result := originalContent
	allApplied := true
	for _, block := range patch.Blocks {
		search := unescapeMarkers(block.Search)
		replace := unescapeMarkers(block.Replace)
		if search == replace {
			continue
		}
		if search == "" {
			allApplied = false
			break
		}
		if !strings.Contains(result, search) {
			allApplied = false
			break
		}
		result = strings.ReplaceAll(result, search, replace)
	}
	if allApplied {
		return result, nil
	}

	// Phase 2: line-based approach.
	strategy := NewMultiSearchReplaceDiffStrategy(0.8, 40)
	matches := make([]diffBlock, 0, len(patch.Blocks))
	for _, block := range patch.Blocks {
		matches = append(matches, diffBlock{
			StartLine:      0,
			SearchContent:  block.Search,
			ReplaceContent: block.Replace,
		})
	}
	sortDiffBlocksByStart(matches)

	lineEnding := "\n"
	if strings.Contains(originalContent, "\r\n") {
		lineEnding = "\r\n"
	}
	resultLines := strings.Split(originalContent, "\n")
	if strings.HasSuffix(originalContent, "\r\n") {
		if len(resultLines) > 0 && resultLines[len(resultLines)-1] == "" {
			resultLines = resultLines[:len(resultLines)-1]
		}
	}
	var diffResults []map[string]any
	appliedCount := 0

	for _, r := range matches {
		search := unescapeMarkers(r.SearchContent)
		replace := unescapeMarkers(r.ReplaceContent)

		hasAllLineNumbers := (EveryLineHasLineNumbers(search) && EveryLineHasLineNumbers(replace)) ||
			(EveryLineHasLineNumbers(search) && strings.TrimSpace(replace) == "")

		if hasAllLineNumbers && r.StartLine == 0 {
			if inferred := ExtractStartLineNumber(search); inferred > 0 {
				r.StartLine = inferred
			}
		}
		if hasAllLineNumbers {
			search = stripLineNumbersContent(search, false)
			replace = stripLineNumbersContent(replace, false)
		}

		if search == replace {
			diffResults = append(diffResults, map[string]any{
				"success": true,
				"message": "Search and replace content are identical - no changes needed",
			})
			continue
		}

		var searchLines, replaceLines []string
		if search != "" {
			searchLines = strings.Split(search, "\n")
		}
		if replace != "" {
			replaceLines = strings.Split(replace, "\n")
		}
		if len(searchLines) == 0 {
			diffResults = append(diffResults, map[string]any{
				"success": false,
				"error":   "Empty search content is not allowed",
			})
			continue
		}

		matchIdx := -1
		bestScore := 0.0
		searchChunk := strings.Join(searchLines, "\n")
		searchStartIdx := 0
		searchEndIdx := len(resultLines)

		if r.StartLine != 0 {
			exactStart := r.StartLine - 1
			searchLen := len(searchLines)
			exactEnd := exactStart + searchLen - 1
			if exactEnd >= len(resultLines) {
				exactEnd = len(resultLines) - 1
			}
			if exactStart >= 0 && exactStart <= exactEnd {
				originalChunk := strings.Join(resultLines[exactStart:exactEnd+1], "\n")
				similarity := GetSimilarity(originalChunk, searchChunk)
				if similarity >= strategy.FuzzyThreshold {
					matchIdx = exactStart
					bestScore = similarity
				} else {
					searchStartIdx = maxInt(0, r.StartLine-(strategy.BufferLines+1))
					searchEndIdx = minInt(len(resultLines), r.StartLine+len(searchLines)+strategy.BufferLines)
				}
			}
		}

		if matchIdx == -1 {
			fr := fuzzySearch(resultLines, searchChunk, searchStartIdx, searchEndIdx)
			matchIdx = fr.BestMatchIdx
			bestScore = fr.BestScore
		}

		if matchIdx == -1 || bestScore < strategy.FuzzyThreshold {
			aggressiveSearch := stripLineNumbersContent(search, true)
			aggressiveReplace := stripLineNumbersContent(replace, true)
			var aggressiveSearchLines []string
			if aggressiveSearch != "" {
				aggressiveSearchLines = strings.Split(aggressiveSearch, "\n")
			}
			aggressiveSearchChunk := strings.Join(aggressiveSearchLines, "\n")
			fr := fuzzySearch(resultLines, aggressiveSearchChunk, searchStartIdx, searchEndIdx)
			if fr.BestMatchIdx != -1 && fr.BestScore >= strategy.FuzzyThreshold {
				matchIdx = fr.BestMatchIdx
				bestScore = fr.BestScore
					search = aggressiveSearch
				replace = aggressiveReplace
				if aggressiveSearch != "" {
					searchLines = strings.Split(aggressiveSearch, "\n")
				} else {
					searchLines = nil
				}
				if aggressiveReplace != "" {
					replaceLines = strings.Split(aggressiveReplace, "\n")
				} else {
					replaceLines = nil
				}
			} else {
				diffResults = append(diffResults, map[string]any{
					"success": false,
					"error":   "No sufficiently similar match found",
				})
				continue
			}
		}

		end := matchIdx + len(searchLines)
		if end > len(resultLines) {
			end = len(resultLines)
		}
		matchedLines := resultLines[matchIdx:end]
		originalIndents := extractIndents(matchedLines)
		searchIndents := extractIndents(searchLines)

		indentedReplaceLines := make([]string, 0, len(replaceLines))
		for i, line := range replaceLines {
			var matchedIndent, searchIndent string
			if i < len(originalIndents) {
				matchedIndent = originalIndents[i]
			} else if len(originalIndents) > 0 {
				matchedIndent = originalIndents[0]
			}
			if i < len(searchIndents) {
				searchIndent = searchIndents[i]
			} else if len(searchIndents) > 0 {
				searchIndent = searchIndents[0]
			}
			currentReplaceIndent := ""
			if m := indentRE.FindString(line); m != "" {
				currentReplaceIndent = m
			}
			relativeLevel := len(currentReplaceIndent) - len(searchIndent)
			var finalIndent string
			if relativeLevel >= 0 {
				finalIndent = matchedIndent + currentReplaceIndent[len(searchIndent):]
			} else {
				k := len(matchedIndent) + relativeLevel
				if k < 0 {
					k = 0
				}
				finalIndent = matchedIndent[:k]
			}
			if strings.TrimSpace(line) == "" {
				indentedReplaceLines = append(indentedReplaceLines, matchedIndent)
			} else {
				content := strings.TrimLeft(line, " \t")
				indentedReplaceLines = append(indentedReplaceLines, finalIndent+content)
			}
		}

		before := resultLines[:matchIdx]
		after := resultLines[end:]
		resultLines = append(append(before, indentedReplaceLines...), after...)
		appliedCount++
	}

	finalContent := strings.Join(resultLines, lineEnding)

	hasFailures := false
	for _, r := range diffResults {
		success, _ := r["success"].(bool)
		if !success {
			hasFailures = true
			break
		}
	}

	if appliedCount == 0 && hasFailures {
		return "", NewPatchParseError("patch application failed: search content not found in original")
	}

	return finalContent, nil
}

// extractIndents returns the leading-whitespace prefix of each line.
func extractIndents(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		m := indentRE.FindString(line)
		out[i] = m
	}
	return out
}

// sortDiffBlocksByStart sorts blocks by StartLine in place.
func sortDiffBlocksByStart(blocks []diffBlock) {
	for i := 1; i < len(blocks); i++ {
		for j := i; j > 0 && blocks[j].StartLine < blocks[j-1].StartLine; j-- {
			blocks[j], blocks[j-1] = blocks[j-1], blocks[j]
		}
	}
}

// minInt3 returns the minimum of three ints.
func minInt3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
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

// formatLineNumber renders n as a decimal string. It is split out so
// tests can stub it if needed.
func formatLineNumber(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + formatLineNumber(-n)
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
