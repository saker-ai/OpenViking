package privacy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestService_Redact_Restore verifies the end-to-end pipeline:
// Redact then Restore returns the original text.
func TestService_Redact_Restore(t *testing.T) {
	srv := New()
	cases := []struct {
		name  string
		input string
	}{
		{"email", "send to foo@bar.com please"},
		{"phone", "call (555) 123-4567 now"},
		{"api_key", "auth: sk-abcdef0123456789abcdef0123456789"},
		{"credit_card", "card 4111 1111 1111 1111 ok"},
		{"ssn", "ssn 123-45-6789 file"},
		{"complex", "user foo@bar.com ph (555) 123-4567 key sk-abcdef0123456789abcdef0123456789 cc 4111 1111 1111 1111 ssn 123-45-6789"},
		{"no_pii", "just some text without any PII"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			redacted, m := srv.Redact(tc.input)
			if tc.input == "" {
				assert.Equal(t, "", redacted)
				return
			}
			restored := srv.Restore(redacted, m)
			assert.Equal(t, tc.input, restored, "round-trip should be identity")
		})
	}
}

// TestService_Redact_ReturnsPerRequestMap verifies that the returned
// PIIMap is fresh per call (per-request isolation).
func TestService_Redact_ReturnsPerRequestMap(t *testing.T) {
	srv := New()
	_, m1 := srv.Redact("email foo@bar.com")
	_, m2 := srv.Redact("email baz@qux.com")
	// Both maps have an [EMAIL_1] entry but with different originals.
	p1, ok := m1.Get("[EMAIL_1]")
	require.True(t, ok)
	p2, ok := m2.Get("[EMAIL_1]")
	require.True(t, ok)
	assert.NotEqual(t, p1.Original, p2.Original)
}

// TestService_Redact_PreservesNonPII verifies that text without PII
// passes through unchanged.
func TestService_Redact_PreservesNonPII(t *testing.T) {
	srv := New()
	input := "the quick brown fox jumps over the lazy dog"
	redacted, m := srv.Redact(input)
	assert.Equal(t, input, redacted)
	assert.Equal(t, 0, len(m.byToken))
}

// TestService_Redact_HandlesUnicode verifies that non-ASCII text is
// handled (byte offsets, not rune offsets, are used).
func TestService_Redact_HandlesUnicode(t *testing.T) {
	srv := New()
	input := "联系 foo@bar.com 详情"
	redacted, m := srv.Redact(input)
	assert.Equal(t, "联系 [EMAIL_1] 详情", redacted)
	restored := srv.Restore(redacted, m)
	assert.Equal(t, input, restored)
}

// TestRedact_PackageLevel verifies the package-level convenience.
func TestRedact_PackageLevel(t *testing.T) {
	redacted, m := Redact("email foo@bar.com")
	assert.Equal(t, "email [EMAIL_1]", redacted)
	restored := Restore(redacted, m)
	assert.Equal(t, "email foo@bar.com", restored)
}

// TestService_Redact_NoPIIInRedactedOutput verifies that the redacted
// output does not contain any of the original PII values.
func TestService_Redact_NoPIIInRedactedOutput(t *testing.T) {
	srv := New()
	input := "email foo@bar.com phone 555-123-4567"
	redacted, _ := srv.Redact(input)
	assert.NotContains(t, redacted, "foo@bar.com")
	assert.NotContains(t, redacted, "555-123-4567")
}

// TestService_Redact_MultipleSameType verifies that distinct PII
// values of the same type get distinct placeholders.
func TestService_Redact_MultipleSameType(t *testing.T) {
	srv := New()
	input := "emails foo@bar.com and baz@qux.com"
	redacted, m := srv.Redact(input)
	assert.Contains(t, redacted, "[EMAIL_1]")
	assert.Contains(t, redacted, "[EMAIL_2]")
	assert.Len(t, m.byToken, 2)
}
