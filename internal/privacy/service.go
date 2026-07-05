package privacy

// Service is the privacy pipeline orchestrator. It is stateless; the
// PIIMap returned by Redact carries all the per-request state.
//
// Callers that just want a one-shot redaction should use the
// package-level Redact / Restore functions, which delegate to a
// default Service. Inject a Service when you want a typed dependency
// (e.g., for testing or for swapping implementations).
type Service struct{}

// New returns a Service. The Service is stateless so a single shared
// instance is safe for concurrent use.
func New() *Service { return &Service{} }

// defaultService is the singleton used by the package-level Redact /
// Restore conveniences.
var defaultService = New()

// Redact extracts PII from text and replaces each occurrence with a
// deterministic placeholder ([EMAIL_1], [PHONE_2], ...). The returned
// PIIMap is the per-request bidirectional map; pass it to Restore to
// reverse the substitution.
//
// The PIIMap is per-request: callers must NOT share it across
// requests, otherwise PII from one request may leak into another.
func (s *Service) Redact(text string) (string, *PIIMap) {
	matches := Extract(text)
	m := NewPIIMap()
	redacted := Substitute(text, matches, m)
	return redacted, m
}

// Restore reverses Redact, substituting placeholders with their
// original PII values. A nil PIIMap makes Restore a no-op.
func (s *Service) Restore(text string, m *PIIMap) string {
	return Restore(text, m)
}

// Redact is a package-level convenience that delegates to a default
// Service. Equivalent to `New().Redact(text)`.
func Redact(text string) (string, *PIIMap) { return defaultService.Redact(text) }
