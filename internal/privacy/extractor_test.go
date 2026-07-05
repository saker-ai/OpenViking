package privacy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtract_Email covers the email regex with a handful of common
// formats including subaddressing and country TLDs.
func TestExtract_Email(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "contact me at foo@bar.com", "foo@bar.com"},
		{"subaddress", "email user.name+tag@example.co.uk", "user.name+tag@example.co.uk"},
		{"dotted", "reach john.doe@acme.io please", "john.doe@acme.io"},
		{"numeric", "msg 12345@mail.example.org", "12345@mail.example.org"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			require.Len(t, matches, 1, "expected one match")
			assert.Equal(t, PIITypeEmail, matches[0].Type)
			assert.Equal(t, tc.want, matches[0].Value)
			assert.True(t, matches[0].Start < matches[0].End)
			assert.Equal(t, tc.want, tc.input[matches[0].Start:matches[0].End])
		})
	}
}

// TestExtract_Email_NoMatch verifies conservative behavior: strings
// that look like emails but lack a TLD or have invalid chars are
// skipped.
func TestExtract_Email_NoMatch(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"missing_tld", "send to foo@bar"},
		{"empty_local", "ping @example.com"},
		{"trailing_dot", "hi foo@bar.com. extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			// "foo@bar.com." may match "foo@bar.com" (without the
			// trailing dot); that's acceptable. We only require that
			// the false-positive cases don't over-trigger.
			for _, m := range matches {
				if m.Type == PIITypeEmail {
					// Acceptable as long as the match is a valid email.
					assert.Regexp(t, `^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`, m.Value)
				}
			}
		})
	}
}

// TestExtract_Phone covers US and international phone formats.
func TestExtract_Phone(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"us_parens", "call (555) 123-4567 now", "(555) 123-4567"},
		{"us_dashes", "call 555-123-4567 now", "555-123-4567"},
		{"intl_plus", "call +1-555-123-4567 now", "+1-555-123-4567"},
		{"intl_dot", "call +86.138.0013.8000 now", "+86.138.0013.8000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			require.NotEmpty(t, matches, "expected at least one match")
			// Find the phone match (the input may also produce other
			// matches in edge cases; assert at least one is a phone).
			var got *PIIMatch
			for i := range matches {
				if matches[i].Type == PIITypePhone {
					got = &matches[i]
					break
				}
			}
			require.NotNil(t, got, "expected a phone match")
			assert.Equal(t, tc.want, got.Value)
		})
	}
}

// TestExtract_APIKey covers the well-known API key prefixes.
func TestExtract_APIKey(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"openai_sk", "key sk-abcdef0123456789abcdef0123456789 here", "sk-abcdef0123456789abcdef0123456789"},
		{"openai_proj", "key sk-proj-abcdef0123456789abcdef0123456789 here", "sk-proj-abcdef0123456789abcdef0123456789"},
		{"aws_akia", "creds AKIAIOSFODNN7EXAMPLE found", "AKIAIOSFODNN7EXAMPLE"},
		{"github_pat", "ghp_abcdefghijklmnopqrstuvwxyz0123456789 token", "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"slack_xoxb", "xoxb-1234567890-abcdefghij token", "xoxb-1234567890-abcdefghij"},
		{"google_aiza", "key AIzaSyA1234567890abcdefghijklmnopqrstuv", "AIzaSyA1234567890abcdefghijklmnopqrstuv"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			require.NotEmpty(t, matches, "expected at least one match")
			var got *PIIMatch
			for i := range matches {
				if matches[i].Type == PIITypeAPIKey {
					got = &matches[i]
					break
				}
			}
			require.NotNil(t, got, "expected an api_key match")
			assert.Equal(t, tc.want, got.Value)
		})
	}
}

// TestExtract_CreditCard covers valid Luhn cards.
func TestExtract_CreditCard(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"visa_spaced", "card 4111 1111 1111 1111 here", "4111 1111 1111 1111"},
		{"mc_spaced", "card 5500 0000 0000 0004 here", "5500 0000 0000 0004"},
		{"amex_dashes", "card 3782-822463-10005 here", "3782-822463-10005"},
		{"no_separator", "card 4111111111111111 here", "4111111111111111"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			require.NotEmpty(t, matches, "expected at least one match")
			var got *PIIMatch
			for i := range matches {
				if matches[i].Type == PIITypeCreditCard {
					got = &matches[i]
					break
				}
			}
			require.NotNil(t, got, "expected a credit_card match")
			assert.Equal(t, tc.want, got.Value)
		})
	}
}

// TestExtract_CreditCard_LuhnReject verifies that 16-digit numbers
// that fail Luhn are not classified as credit cards.
func TestExtract_CreditCard_LuhnReject(t *testing.T) {
	// 16-digit number that fails Luhn (sequential digits).
	matches := Extract("ref 1234567890123456 done")
	for _, m := range matches {
		if m.Type == PIITypeCreditCard {
			t.Errorf("Luhn-failing number was classified as credit card: %+v", m)
		}
	}
}

// TestExtract_SSN covers US Social Security Numbers.
func TestExtract_SSN(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"standard", "ssn 123-45-6789 here", "123-45-6789"},
		{"leading_zero", "ssn 001-02-0003 here", "001-02-0003"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			require.NotEmpty(t, matches, "expected at least one match")
			var got *PIIMatch
			for i := range matches {
				if matches[i].Type == PIITypeSSN {
					got = &matches[i]
					break
				}
			}
			require.NotNil(t, got, "expected an ssn match")
			assert.Equal(t, tc.want, got.Value)
		})
	}
}

// TestExtract_IBAN covers real-world IBANs that pass mod-97.
func TestExtract_IBAN(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"gb", "iban GB82WEST12345698765432 here", "GB82WEST12345698765432"},
		{"de", "iban DE89370400440532013000 here", "DE89370400440532013000"},
		{"fr", "iban FR1420041010050500013M02606 here", "FR1420041010050500013M02606"},
		{"ch", "iban CH9300762011623852957 here", "CH9300762011623852957"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			require.NotEmpty(t, matches, "expected at least one match")
			var got *PIIMatch
			for i := range matches {
				if matches[i].Type == PIITypeIBAN {
					got = &matches[i]
					break
				}
			}
			require.NotNil(t, got, "expected an iban match")
			assert.Equal(t, tc.want, got.Value)
		})
	}
}

// TestExtract_IBAN_Mod97Reject verifies that IBAN-shaped strings
// failing the mod-97 checksum are not classified as IBAN.
func TestExtract_IBAN_Mod97Reject(t *testing.T) {
	// Same shape as a real IBAN but with a wrong check digit.
	matches := Extract("iban GB00WEST12345698765432 done")
	for _, m := range matches {
		if m.Type == PIITypeIBAN {
			t.Errorf("mod-97-failing string was classified as IBAN: %+v", m)
		}
	}
}

// TestExtract_SWIFT covers 8- and 11-character SWIFT/BIC codes.
func TestExtract_SWIFT(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"8char", "swift DEUTDEFF here", "DEUTDEFF"},
		{"11char", "swift DEUTDEFF500 here", "DEUTDEFF500"},
		{"with_branch_x", "bic CHASUSUSXXX here", "CHASUSUSXXX"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			require.NotEmpty(t, matches, "expected at least one match")
			var got *PIIMatch
			for i := range matches {
				if matches[i].Type == PIITypeSWIFT {
					got = &matches[i]
					break
				}
			}
			require.NotNil(t, got, "expected a swift match")
			assert.Equal(t, tc.want, got.Value)
		})
	}
}

// TestExtract_IPv4 covers dotted-quad IPv4 addresses.
func TestExtract_IPv4(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"private", "ip 192.168.1.1 here", "192.168.1.1"},
		{"loopback", "ip 127.0.0.1 here", "127.0.0.1"},
		{"max_octet", "ip 255.255.255.255 here", "255.255.255.255"},
		{"zero_padded", "ip 10.0.0.1 here", "10.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			require.NotEmpty(t, matches, "expected at least one match")
			var got *PIIMatch
			for i := range matches {
				if matches[i].Type == PIITypeIPv4 {
					got = &matches[i]
					break
				}
			}
			require.NotNil(t, got, "expected an ipv4 match")
			assert.Equal(t, tc.want, got.Value)
		})
	}
}

// TestExtract_IPv4_OutOfRange verifies that octets > 255 are not
// matched.
func TestExtract_IPv4_OutOfRange(t *testing.T) {
	matches := Extract("ip 256.1.1.1 here")
	for _, m := range matches {
		if m.Type == PIITypeIPv4 {
			t.Errorf("out-of-range octet was classified as IPv4: %+v", m)
		}
	}
}

// TestExtract_IPv6 covers full-length IPv6 addresses.
func TestExtract_IPv6(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"full", "ip 2001:0db8:85a3:0000:0000:8a2e:0370:7334 here", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"},
		{"short_groups", "ip fe80:1:2:3:4:5:6:7 here", "fe80:1:2:3:4:5:6:7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := Extract(tc.input)
			require.NotEmpty(t, matches, "expected at least one match")
			var got *PIIMatch
			for i := range matches {
				if matches[i].Type == PIITypeIPv6 {
					got = &matches[i]
					break
				}
			}
			require.NotNil(t, got, "expected an ipv6 match")
			assert.Equal(t, tc.want, got.Value)
		})
	}
}

// TestExtract_Overlaps verifies that overlapping matches are resolved
// by priority: a phone-shaped substring inside a credit card should
// not produce a phone match (and vice versa).
func TestExtract_Overlaps(t *testing.T) {
	// "4111 1111 1111 1111" is a valid Visa. Without overlap
	// resolution, the phone regex could match "1111 1111 1111" or
	// similar. We expect exactly one match for the whole card.
	matches := Extract("card 4111 1111 1111 1111 end")
	var cc, phone int
	for _, m := range matches {
		switch m.Type {
		case PIITypeCreditCard:
			cc++
		case PIITypePhone:
			phone++
		}
	}
	assert.Equal(t, 1, cc, "expected one credit card match")
	assert.Equal(t, 0, phone, "expected no overlapping phone match")
}

// TestExtract_MultiplePII verifies that multiple distinct PII values
// in the same text are all returned.
func TestExtract_MultiplePII(t *testing.T) {
	input := "email foo@bar.com, phone 555-123-4567, ssn 123-45-6789"
	matches := Extract(input)
	types := map[PIIType]int{}
	for _, m := range matches {
		types[m.Type]++
	}
	assert.Equal(t, 1, types[PIITypeEmail], "want 1 email, got %d", types[PIITypeEmail])
	assert.Equal(t, 1, types[PIITypePhone], "want 1 phone, got %d", types[PIITypePhone])
	assert.Equal(t, 1, types[PIITypeSSN], "want 1 ssn, got %d", types[PIITypeSSN])
}

// TestExtract_Empty verifies that empty input returns nil.
func TestExtract_Empty(t *testing.T) {
	assert.Nil(t, Extract(""))
}

// TestExtract_NoFalsePositives verifies that common non-PII strings
// don't trigger matches.
func TestExtract_NoFalsePositives(t *testing.T) {
	cases := []string{
		"the quick brown fox",
		"version 1.2.3 released",
		"see chapters 1-3 and 4-6",
		"room 101, floor 2",
	}
	for _, input := range cases {
		matches := Extract(input)
		for _, m := range matches {
			t.Errorf("false positive for input %q: %+v", input, m)
		}
	}
}
