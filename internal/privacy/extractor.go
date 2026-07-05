package privacy

import (
	"math/big"
	"regexp"
	"sort"
	"strings"
)

// patterns is the list of (type, regex) pairs used by Extract. Order
// matters for overlap resolution: more specific patterns must come
// before more general ones. Credit card is placed before phone so a
// Luhn-valid 16-digit card wins over a sub-span phone-shaped match.
// IBAN/SWIFT/IPv4/IPv6 are placed before phone so their digit spans
// are not absorbed by the more permissive phone regex.
//
// Patterns are intentionally conservative (per the task brief: "better
// to miss a PII than to over-redact"). Specific intentional skips:
//
//   - US passport / driver license: state-specific, low signal.
//   - Generic NER (person names, addresses): requires a Python-only
//     NER model (spaCy / presidio). Documented as a gap; not stubbed.
var patterns = []struct {
	Type PIIType
	Re   *regexp.Regexp
}{
	{PIITypeEmail, emailRE},
	{PIITypeAPIKey, apiKeyRE},
	{PIITypeSSN, ssnRE},
	{PIITypeCreditCard, creditCardRE},
	{PIITypeIBAN, ibanRE},
	{PIITypeSWIFT, swiftRE},
	{PIITypeIPv4, ipv4RE},
	{PIITypeIPv6, ipv6RE},
	{PIITypePhone, phoneRE},
}

var (
	// emailRE matches common email addresses. Conservative: requires
	// a TLD of at least 2 letters and disallows quoted local parts.
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]{1,64}@[A-Za-z0-9.\-]{1,253}\.[A-Za-z]{2,24}`)

	// apiKeyRE matches common API key formats:
	//   - OpenAI: sk-..., sk-proj-..., sk-svcacct-...
	//   - AWS access keys: AKIA... (16 chars after prefix)
	//   - GitHub: ghp_..., ghs_..., github_pat_...
	//   - Slack: xox[bp]-...
	//   - Stripe: sk_live_..., sk_test_..., rk_live_..., rk_test_...
	//   - Google API: AIza... (35 chars)
	// Conservative: requires well-known prefixes to avoid matching
	// arbitrary alphanumeric strings.
	apiKeyRE = regexp.MustCompile(`(?:sk-proj-[A-Za-z0-9_\-]{20,200}|sk-svcacct-[A-Za-z0-9_\-]{20,200}|sk-[A-Za-z0-9]{20,200}|AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{36}|ghs_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,82}|xox[bp]-[A-Za-z0-9\-]{10,72}|sk_(?:live|test)_[A-Za-z0-9]{16,128}|rk_(?:live|test)_[A-Za-z0-9]{16,128}|AIza[0-9A-Za-z\-]{27,80})`)

	// phoneRE matches international and US phone numbers. Requires
	// either a +CC prefix or a 10-digit US format with separators.
	// Conservative: requires explicit separators (space, dash, dot)
	// or parens around the area code to avoid matching credit cards
	// and other long numeric strings.
	phoneRE = regexp.MustCompile(`(?:\+\d{1,3}[\s.\-])?(?:\(\d{2,4}\)[\s.\-]?\d{3,4}[\s.\-]\d{3,4}|\d{2,4}[\s.\-]\d{3,4}[\s.\-]\d{3,4})`)

	// ssnRE matches US Social Security Numbers (XXX-XX-XXXX). Requires
	// the dash separators; bare 9-digit numbers are too ambiguous.
	ssnRE = regexp.MustCompile(`\d{3}-\d{2}-\d{4}`)

	// creditCardRE matches 13-19 digit numbers with optional space or
	// dash separators. Conservative: requires at least 13 digits to
	// avoid matching phone numbers; a Luhn check filters false
	// positives (e.g., sequential IDs).
	creditCardRE = regexp.MustCompile(`(?:\d[ -]?){13,19}`)

	// ibanRE matches International Bank Account Numbers. Format:
	//   2-letter country code (ISO 3166-1 alpha-2)
	//   2-digit check digits
	//   11-30 alphanumeric BBAN chars
	// Conservative: requires the leading country code + check digits
	// shape; a mod-97 checksum filter drops false positives.
	// Spaces are NOT accepted inline (IBANs are commonly written with
	// spaces, but a single regex covering both forms would risk
	// matching phone numbers).
	ibanRE = regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}\b`)

	// swiftRE matches SWIFT/BIC codes. Format:
	//   4-letter bank code
	//   2-letter country code
	//   2-char location code (alphanumeric)
	//   optional 3-char branch code (alphanumeric)
	// Total length is 8 or 11. Word-bounded to reduce false positives.
	swiftRE = regexp.MustCompile(`\b[A-Z]{4}[A-Z]{2}[A-Z0-9]{2}(?:[A-Z0-9]{3})?\b`)

	// ipv4RE matches dotted-quad IPv4 addresses. Each octet must be
	// 0-255; word-bounded to avoid matching version strings.
	ipv4RE = regexp.MustCompile(`\b(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)(?:\.(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)){3}\b`)

	// ipv6RE matches full-length IPv6 addresses (8 groups of 1-4 hex
	// digits separated by colons). The compressed `::` form is not
	// matched to avoid noise (e.g. "::1" matches too many things).
	ipv6RE = regexp.MustCompile(`\b[0-9a-fA-F]{1,4}(?::[0-9a-fA-F]{1,4}){7}\b`)
)

// Extract finds all PII matches in text. Matches are returned in
// left-to-right order; overlapping matches are resolved by priority
// (the first pattern in `patterns` wins). For credit cards, a Luhn
// check is applied to filter false positives. For IBANs, a mod-97
// checksum check is applied.
//
// Extract is the lowest-level entry point; callers usually want
// Service.Redact or the package-level Redact convenience.
func Extract(text string) []PIIMatch {
	if text == "" {
		return nil
	}
	var matches []PIIMatch
	for _, p := range patterns {
		for _, m := range p.Re.FindAllStringIndex(text, -1) {
			if m[0] < 0 || m[1] > len(text) || m[0] >= m[1] {
				continue
			}
			val := text[m[0]:m[1]]
			if p.Type == PIITypeCreditCard {
				// Luhn check: drop non-Luhn numbers.
				if !luhnValid(stripSeparators(val)) {
					continue
				}
				// Trim trailing separators captured by the regex.
				val = strings.TrimRight(val, " -")
			}
			if p.Type == PIITypeIBAN {
				// mod-97 check: drop IBANs that fail the checksum.
				if !ibanValid(val) {
					continue
				}
			}
			matches = append(matches, PIIMatch{
				Type:  p.Type,
				Value: val,
				Start: m[0],
				End:   m[0] + len(val),
			})
		}
	}
	return resolveOverlaps(matches)
}

// resolveOverlaps drops any match whose byte range overlaps with an
// earlier-priority (lower index in `patterns`) match. The input is
// not assumed to be sorted; the output is sorted by Start ascending.
func resolveOverlaps(matches []PIIMatch) []PIIMatch {
	if len(matches) <= 1 {
		return matches
	}
	// Sort by priority (patterns index) then by Start so we can
	// iterate and drop overlaps against higher-priority matches.
	prio := map[PIIType]int{}
	for i, p := range patterns {
		prio[p.Type] = i
	}
	sort.SliceStable(matches, func(i, j int) bool {
		pi, pj := prio[matches[i].Type], prio[matches[j].Type]
		if pi != pj {
			return pi < pj
		}
		return matches[i].Start < matches[j].Start
	})
	var keep []PIIMatch
	for _, m := range matches {
		drop := false
		for _, k := range keep {
			if overlaps(m.Start, m.End, k.Start, k.End) {
				drop = true
				break
			}
		}
		if !drop {
			keep = append(keep, m)
		}
	}
	// Re-sort by Start so callers get left-to-right order.
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].Start < keep[j].Start })
	return keep
}

// overlaps reports whether [a0, a1) and [b0, b1) overlap.
func overlaps(a0, a1, b0, b1 int) bool {
	return a0 < b1 && b0 < a1
}

// stripSeparators removes spaces and dashes from a credit card string
// before the Luhn check.
func stripSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == ' ' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// luhnValid reports whether digits passes the Luhn checksum. Used to
// filter false-positive credit card matches.
func luhnValid(digits string) bool {
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		c := digits[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// ibanValid reports whether s passes the IBAN mod-97 checksum. The
// input must be a bare alphanumeric IBAN (no spaces). Algorithm:
//  1. Move the first 4 chars (country + check digits) to the end.
//  2. Replace each letter with its decimal value (A=10, B=11, ..., Z=35).
//  3. Interpret the result as a single big integer mod 97; valid IBANs
//     have remainder 1.
//
// Used to filter false-positive IBAN matches (the regex shape is
// permissive; the checksum is what makes an IBAN real).
func ibanValid(s string) bool {
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	// Step 1: rotate first 4 chars to the end.
	rotated := s[4:] + s[:4]
	// Step 2: letters -> decimal values.
	var b strings.Builder
	b.Grow(len(rotated) * 2)
	for _, r := range rotated {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			// A=10, B=11, ..., Z=35.
			val := 10 + int(r-'A')
			b.WriteString(itoa(val))
		default:
			return false
		}
	}
	// Step 3: mod 97 == 1.
	n, ok := new(big.Int).SetString(b.String(), 10)
	if !ok {
		return false
	}
	rem := new(big.Int).Mod(n, big.NewInt(97))
	return rem.Int64() == 1
}

// itoa returns the decimal string for a non-negative int. Avoids
// pulling in strconv for a tiny helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
