package privacy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// BuildPlaceholder returns the deterministic placeholder string for a
// given PII type and 1-indexed counter. Example:
//
//	BuildPlaceholder(PIITypeEmail, 1) -> "[EMAIL_1]"
//	BuildPlaceholder(PIITypeAPIKey, 3) -> "[API_KEY_3]"
//
// The format is "[<TYPE>_<N>]" where TYPE is the upper-case PIIType
// string and N is the 1-indexed counter. The token matches
// placeholderTokenRE so Restore can find it later.
func BuildPlaceholder(t PIIType, n int) string {
	return fmt.Sprintf("[%s_%d]", strings.ToUpper(string(t)), n)
}

// placeholderTokenRE matches any placeholder token produced by
// BuildPlaceholder. Used by Restore to find tokens to replace.
var placeholderTokenRE = regexp.MustCompile(`\[(?:EMAIL|PHONE|API_KEY|CREDIT_CARD|SSN)_\d+\]`)

// add inserts a new placeholder for the given PII value, or returns
// the existing token if the value is already mapped. The per-type
// counter is only incremented for new values, so repeated occurrences
// of the same value share a token.
func (m *PIIMap) add(t PIIType, value string) string {
	if tok, ok := m.byValue[value]; ok {
		return tok
	}
	m.counters[t]++
	tok := BuildPlaceholder(t, m.counters[t])
	m.byToken[tok] = Placeholder{Token: tok, Original: value, Type: t}
	m.byValue[value] = tok
	return tok
}

// Substitute replaces all PII matches in text with their placeholders,
// populating the PIIMap as a side effect. Matches are processed in
// two passes:
//
//  1. Tokens are assigned in left-to-right order so the first
//     occurrence of a PII value gets the lowest counter for its type.
//  2. Substitutions are applied in right-to-left order so byte
//     offsets remain valid as the string is rebuilt.
//
// Matches MUST be non-overlapping (callers should pass the output of
// Extract, which already resolves overlaps). Overlapping matches will
// produce undefined output.
func Substitute(text string, matches []PIIMatch, m *PIIMap) string {
	if m == nil || len(matches) == 0 {
		return text
	}
	// Pass 1: assign tokens in left-to-right order so numbering is
	// intuitive (first occurrence in text -> lowest counter).
	sorted := make([]PIIMatch, len(matches))
	copy(sorted, matches)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	for _, mm := range sorted {
		m.add(mm.Type, mm.Value)
	}
	// Pass 2: apply substitutions in right-to-left order so earlier
	// offsets stay valid as we splice.
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start > sorted[j].Start })
	out := text
	for _, mm := range sorted {
		if mm.Start < 0 || mm.End > len(out) || mm.Start >= mm.End {
			continue
		}
		tok, _ := m.TokenFor(mm.Value)
		if tok == "" {
			continue
		}
		out = out[:mm.Start] + tok + out[mm.End:]
	}
	return out
}

// sortPlaceholders sorts a slice of Placeholder by Token ascending.
// Used by PIIMap.Placeholders for deterministic output.
func sortPlaceholders(out []Placeholder) {
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
}
