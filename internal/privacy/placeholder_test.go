package privacy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildPlaceholder verifies the placeholder format for each PII
// type. The format is "[<TYPE>_<N>]" with TYPE upper-cased.
func TestBuildPlaceholder(t *testing.T) {
	cases := []struct {
		typ PIIType
		n   int
		want string
	}{
		{PIITypeEmail, 1, "[EMAIL_1]"},
		{PIITypeEmail, 5, "[EMAIL_5]"},
		{PIITypePhone, 1, "[PHONE_1]"},
		{PIITypeAPIKey, 1, "[API_KEY_1]"},
		{PIITypeCreditCard, 1, "[CREDIT_CARD_1]"},
		{PIITypeSSN, 1, "[SSN_1]"},
	}
	for _, tc := range cases {
		t.Run(string(tc.typ), func(t *testing.T) {
			assert.Equal(t, tc.want, BuildPlaceholder(tc.typ, tc.n))
		})
	}
}

// TestSubstitute_RoundTrip verifies that Substitute then Restore
// returns the original text.
func TestSubstitute_RoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"email", "contact foo@bar.com for info"},
		{"phone", "call (555) 123-4567 today"},
		{"api_key", "use sk-abcdef0123456789abcdef0123456789 for auth"},
		{"credit_card", "card 4111 1111 1111 1111 expires 12/25"},
		{"ssn", "ssn 123-45-6789 on file"},
		{"multi", "email foo@bar.com, phone 555-123-4567, ssn 123-45-6789"},
		{"none", "no PII here at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			m := NewPIIMap()
			redacted := Substitute(tc.input, matches, m)
			restored := Restore(redacted, m)
			assert.Equal(t, tc.input, restored, "round-trip should be identity")
		})
	}
}

// TestSubstitute_Deterministic verifies that the same input produces
// the same redacted output across calls (per-request PIIMap, but
// deterministic within a request).
func TestSubstitute_Deterministic(t *testing.T) {
	input := "email foo@bar.com then bar@baz.com and foo@bar.com again"
	matches := Extract(input)
	m1 := NewPIIMap()
	r1 := Substitute(input, matches, m1)
	m2 := NewPIIMap()
	r2 := Substitute(input, matches, m2)
	assert.Equal(t, r1, r2, "redacted output should be deterministic")
	// The first occurrence of foo@bar.com should be EMAIL_1, bar@baz.com
	// should be EMAIL_2, and the second foo@bar.com should reuse EMAIL_1.
	assert.Equal(t, "email [EMAIL_1] then [EMAIL_2] and [EMAIL_1] again", r1)
}

// TestSubstitute_PerRequestIsolation verifies that two PIIMaps
// generated for different inputs don't share state. This is the
// "per-request" guarantee the task brief calls out.
func TestSubstitute_PerRequestIsolation(t *testing.T) {
	input1 := "email foo@bar.com"
	input2 := "email baz@qux.com"
	m1 := NewPIIMap()
	r1 := Substitute(input1, Extract(input1), m1)
	m2 := NewPIIMap()
	r2 := Substitute(input2, Extract(input2), m2)
	// Both get [EMAIL_1] in their own map.
	assert.Equal(t, "email [EMAIL_1]", r1)
	assert.Equal(t, "email [EMAIL_1]", r2)
	// m1's EMAIL_1 maps to foo@bar.com; m2's EMAIL_1 maps to baz@qux.com.
	p1, ok := m1.Get("[EMAIL_1]")
	require.True(t, ok)
	assert.Equal(t, "foo@bar.com", p1.Original)
	p2, ok := m2.Get("[EMAIL_1]")
	require.True(t, ok)
	assert.Equal(t, "baz@qux.com", p2.Original)
	// Cross-restore: m1 should NOT restore r2 (different map).
	assert.NotEqual(t, input2, Restore(r2, m1), "cross-request leak: m1 restored r2")
}

// TestSubstitute_RepeatedValue verifies that the same PII value
// appearing multiple times gets the same placeholder.
func TestSubstitute_RepeatedValue(t *testing.T) {
	input := "foo@bar.com foo@bar.com foo@bar.com"
	m := NewPIIMap()
	redacted := Substitute(input, Extract(input), m)
	assert.Equal(t, "[EMAIL_1] [EMAIL_1] [EMAIL_1]", redacted)
	// Only one placeholder entry despite three occurrences.
	assert.Len(t, m.byToken, 1)
}

// TestRestore_NilMap verifies that a nil PIIMap is a no-op.
func TestRestore_NilMap(t *testing.T) {
	assert.Equal(t, "hello [EMAIL_1]", Restore("hello [EMAIL_1]", nil))
}

// TestRestore_PartialMap verifies that tokens missing from the map
// are left in place.
func TestRestore_PartialMap(t *testing.T) {
	m := NewPIIMap()
	m.add(PIITypeEmail, "foo@bar.com")
	// Only [EMAIL_1] is in the map; [PHONE_1] should be left alone.
	out := Restore("email [EMAIL_1] phone [PHONE_1]", m)
	assert.Equal(t, "email foo@bar.com phone [PHONE_1]", out)
}

// TestPIIMap_TokenFor verifies the bidirectional lookup.
func TestPIIMap_TokenFor(t *testing.T) {
	m := NewPIIMap()
	m.add(PIITypeEmail, "foo@bar.com")
	tok, ok := m.TokenFor("foo@bar.com")
	require.True(t, ok)
	assert.Equal(t, "[EMAIL_1]", tok)
	_, ok = m.TokenFor("not@here.com")
	assert.False(t, ok)
}

// TestPIIMap_Placeholders verifies deterministic ordering.
func TestPIIMap_Placeholders(t *testing.T) {
	m := NewPIIMap()
	m.add(PIITypeEmail, "foo@bar.com")
	m.add(PIITypePhone, "555-123-4567")
	m.add(PIITypeSSN, "123-45-6789")
	got := m.Placeholders()
	require.Len(t, got, 3)
	// Sorted by Token ascending; each type has its own counter.
	assert.Equal(t, "[EMAIL_1]", got[0].Token)
	assert.Equal(t, "[PHONE_1]", got[1].Token)
	assert.Equal(t, "[SSN_1]", got[2].Token)
}
