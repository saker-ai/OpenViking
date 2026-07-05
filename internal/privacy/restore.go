package privacy

// Restore replaces all placeholder tokens in text with their original
// PII values from the map. Tokens with no entry in the map are left
// in place (so partial restores are safe — only PII values that were
// redacted in the same request get restored).
//
// Restore is the inverse of Substitute / Service.Redact. A nil or
// empty PIIMap makes Restore a no-op.
func Restore(text string, m *PIIMap) string {
	if m == nil || len(m.byToken) == 0 || text == "" {
		return text
	}
	return placeholderTokenRE.ReplaceAllStringFunc(text, func(tok string) string {
		if p, ok := m.Get(tok); ok {
			return p.Original
		}
		return tok
	})
}
