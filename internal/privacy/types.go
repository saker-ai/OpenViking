package privacy

// PIIType enumerates the kinds of PII the extractor can detect. The
// string form is used as the placeholder prefix (e.g. PIITypeEmail ->
// "[EMAIL_1]").
type PIIType string

const (
	// PIITypeEmail matches common email addresses.
	PIITypeEmail PIIType = "email"
	// PIITypePhone matches international and US phone numbers.
	PIITypePhone PIIType = "phone"
	// PIITypeAPIKey matches common API key formats (OpenAI, AWS,
	// GitHub, Slack, Stripe, Google).
	PIITypeAPIKey PIIType = "api_key"
	// PIITypeCreditCard matches 13-19 digit numbers that pass a Luhn
	// check.
	PIITypeCreditCard PIIType = "credit_card"
	// PIITypeSSN matches US Social Security Numbers (XXX-XX-XXXX).
	PIITypeSSN PIIType = "ssn"
	// PIITypeIBAN matches International Bank Account Numbers (e.g.
	// "GB82WEST12345698765432"). Validated via mod-97 checksum.
	PIITypeIBAN PIIType = "iban"
	// PIITypeSWIFT matches SWIFT/BIC codes (8 or 11 chars: 4-letter
	// bank code + 2-letter country + 2-char location + optional 3-char
	// branch).
	PIITypeSWIFT PIIType = "swift"
	// PIITypeIPv4 matches dotted-quad IPv4 addresses. Each octet must
	// be 0-255; word-bounded to reduce false positives on version
	// strings.
	PIITypeIPv4 PIIType = "ipv4"
	// PIITypeIPv6 matches full-length IPv6 addresses (8 groups of 1-4
	// hex digits). The compressed `::` form is not matched to avoid
	// noise.
	PIITypeIPv6 PIIType = "ipv6"
)

// PIIMatch is a single detected PII occurrence in text. Start and End
// are byte offsets into the input text (End is exclusive).
type PIIMatch struct {
	// Type is the kind of PII detected.
	Type PIIType
	// Value is the matched PII text.
	Value string
	// Start is the byte offset of the first byte of the match.
	Start int
	// End is the byte offset one past the last byte of the match.
	End int
}

// Placeholder is a single placeholder -> original mapping stored in a
// PIIMap.
type Placeholder struct {
	// Token is the placeholder string, e.g. "[EMAIL_1]".
	Token string
	// Original is the PII value that was replaced.
	Original string
	// Type is the kind of PII.
	Type PIIType
}

// PIIMap is the per-request bidirectional map between placeholders and
// the original PII values. It is NOT safe for concurrent use across
// requests; one PIIMap per Redact call.
//
// The zero value is not usable; use NewPIIMap.
type PIIMap struct {
	byToken  map[string]Placeholder
	byValue  map[string]string
	counters map[PIIType]int
}

// NewPIIMap returns an empty PIIMap ready for use.
func NewPIIMap() *PIIMap {
	return &PIIMap{
		byToken:  make(map[string]Placeholder),
		byValue:  make(map[string]string),
		counters: make(map[PIIType]int),
	}
}

// Get returns the placeholder entry for a given token, if any.
func (m *PIIMap) Get(token string) (Placeholder, bool) {
	if m == nil {
		return Placeholder{}, false
	}
	p, ok := m.byToken[token]
	return p, ok
}

// TokenFor returns the placeholder token for a given PII value, if any.
// The same PII value always maps to the same token within a PIIMap.
func (m *PIIMap) TokenFor(value string) (string, bool) {
	if m == nil {
		return "", false
	}
	t, ok := m.byValue[value]
	return t, ok
}

// Placeholders returns all placeholders in the map, sorted by token
// for deterministic ordering.
func (m *PIIMap) Placeholders() []Placeholder {
	if m == nil {
		return nil
	}
	out := make([]Placeholder, 0, len(m.byToken))
	for _, p := range m.byToken {
		out = append(out, p)
	}
	sortPlaceholders(out)
	return out
}

// PrivacyRequest is the input to the privacy service. Currently a
// placeholder type — the service accepts a plain string for the
// common case and a PrivacyRequest for callers that need to scope
// extraction (e.g., only emails).
type PrivacyRequest struct {
	// Text is the input to redact.
	Text string
	// SkillName, when non-empty, enables the skill-placeholder scheme
	// ({{ov_privacy:skill:NAME:FIELD}}) instead of the default
	// [TYPE_N] scheme.
	SkillName string
	// Categories restricts extraction to the listed PII types. Empty
	// means "all types".
	Categories []PIIType
}

// PrivacyResult is the output of a redaction operation.
type PrivacyResult struct {
	// Redacted is the text with PII replaced by placeholders.
	Redacted string
	// Map is the per-request bidirectional map; pass it to Restore to
	// reverse the substitution.
	Map *PIIMap
	// Matches is the list of PII matches found in the input text.
	Matches []PIIMatch
}
