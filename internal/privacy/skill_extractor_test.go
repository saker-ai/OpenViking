package privacy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildSkillPlaceholder verifies the {{ov_privacy:skill:NAME:FIELD}}
// format.
func TestBuildSkillPlaceholder(t *testing.T) {
	assert.Equal(t, "{{ov_privacy:skill:my-skill:user_email}}", BuildSkillPlaceholder("my-skill", "user_email"))
	assert.Equal(t, "{{ov_privacy:skill:auth:token}}", BuildSkillPlaceholder("auth", "token"))
}

// TestPlaceholderizeSkillContent_YAML verifies placeholderize on a
// YAML-like key: value block.
func TestPlaceholderizeSkillContent_YAML(t *testing.T) {
	content := `name: my-skill
user_email: foo@bar.com
api_key: sk-abcdef0123456789abcdef0123456789
notes: no secrets here
`
	values := map[string]string{
		"email": "foo@bar.com",
		"key":   "sk-abcdef0123456789abcdef0123456789",
	}
	out := PlaceholderizeSkillContent(content, "my-skill", values)
	assert.Contains(t, out, "{{ov_privacy:skill:my-skill:email}}")
	assert.Contains(t, out, "{{ov_privacy:skill:my-skill:key}}")
	assert.NotContains(t, out, "foo@bar.com")
	assert.NotContains(t, out, "sk-abcdef0123456789abcdef0123456789")
	assert.Contains(t, out, "no secrets here")
}

// TestPlaceholderizeSkillContent_JSON verifies placeholderize on a
// JSON-like quoted value block.
func TestPlaceholderizeSkillContent_JSON(t *testing.T) {
	content := `{"email": "foo@bar.com", "name": "test"}`
	values := map[string]string{
		"email": "foo@bar.com",
	}
	out := PlaceholderizeSkillContent(content, "cfg", values)
	assert.Contains(t, out, `"email": "{{ov_privacy:skill:cfg:email}}"`)
	assert.NotContains(t, out, "foo@bar.com")
}

// TestPlaceholderizeSkillContent_Assignment verifies placeholderize
// on an ini/shell-style assignment.
func TestPlaceholderizeSkillContent_Assignment(t *testing.T) {
	content := "API_KEY=sk-abcdef0123456789abcdef0123456789\nNAME=test"
	values := map[string]string{
		"key": "sk-abcdef0123456789abcdef0123456789",
	}
	out := PlaceholderizeSkillContent(content, "env", values)
	assert.Contains(t, out, "API_KEY={{ov_privacy:skill:env:key}}")
	assert.NotContains(t, out, "sk-abcdef0123456789abcdef0123456789")
}

// TestPlaceholderizeSkillContent_NoMatch verifies that values not
// present in the content are not added to ReplacedValues.
func TestPlaceholderizeSkillContent_NoMatch(t *testing.T) {
	content := "name: my-skill\nnotes: nothing here"
	values := map[string]string{
		"missing": "not-in-content@nowhere.com",
	}
	res := PlaceholderizeSkillContentWithBlocks(content, "s", values)
	assert.Empty(t, res.ReplacedValues)
	assert.Empty(t, res.OriginalContentBlocks)
	assert.Equal(t, content, res.SanitizedContent)
}

// TestPlaceholderizeSkillContent_LongerFirst verifies that longer
// values are replaced before shorter ones (avoids partial-replace).
func TestPlaceholderizeSkillContent_LongerFirst(t *testing.T) {
	content := "key: sk-abcdef0123456789abcdef0123456789 and short: sk-short"
	values := map[string]string{
		"long":  "sk-abcdef0123456789abcdef0123456789",
		"short": "sk-short",
	}
	out := PlaceholderizeSkillContent(content, "s", values)
	assert.Contains(t, out, "{{ov_privacy:skill:s:long}}")
	assert.Contains(t, out, "{{ov_privacy:skill:s:short}}")
	assert.NotContains(t, out, "sk-abcdef0123456789abcdef0123456789")
}

// TestRestoreSkillContent_Basic verifies the inverse of placeholderize.
func TestRestoreSkillContent_Basic(t *testing.T) {
	content := `name: my-skill
email: {{ov_privacy:skill:my-skill:email}}
`
	values := map[string]string{
		"email": "foo@bar.com",
	}
	out := RestoreSkillContent(content, "my-skill", values)
	assert.Contains(t, out, "email: foo@bar.com")
	assert.NotContains(t, out, "{{ov_privacy:")
}

// TestRestoreSkillContent_MissingValue verifies the [Privacy Config
// Notice] block when a placeholder has no value.
func TestRestoreSkillContent_MissingValue(t *testing.T) {
	content := "email: {{ov_privacy:skill:s:email}}"
	values := map[string]string{}
	out := RestoreSkillContent(content, "s", values)
	assert.Contains(t, out, "[Privacy Config Notice]")
	assert.Contains(t, out, "email=<missing>")
}

// TestRestoreSkillContent_ExtraValue verifies the notice for values
// not referenced in the content.
func TestRestoreSkillContent_ExtraValue(t *testing.T) {
	content := "name: just-name"
	values := map[string]string{
		"unused": "foo@bar.com",
	}
	out := RestoreSkillContent(content, "s", values)
	assert.Contains(t, out, "[Privacy Config Notice]")
	assert.Contains(t, out, "unused=foo@bar.com")
}

// TestExtractSkillPrivacyValues_RegexApproximation verifies the regex
// approximation of the Python LLM-based extraction.
func TestExtractSkillPrivacyValues_RegexApproximation(t *testing.T) {
	content := `name: my-skill
user_email: foo@bar.com
api_key: sk-abcdef0123456789abcdef0123456789
notes: no secrets
`
	res := ExtractSkillPrivacyValues("my-skill", "test skill", content)
	assert.Equal(t, content, res.OriginalContent)
	// The two PII values should be replaced with placeholders.
	assert.NotContains(t, res.SanitizedContent, "foo@bar.com")
	assert.NotContains(t, res.SanitizedContent, "sk-abcdef0123456789abcdef0123456789")
	assert.Contains(t, res.SanitizedContent, "{{ov_privacy:skill:my-skill:field_")
	// Both values should be in ReplacedValues.
	require.Len(t, res.Values, 2)
	// Round-trip: restore should bring back the originals.
	restored := RestoreSkillContent(res.SanitizedContent, "my-skill", res.Values)
	assert.Contains(t, restored, "foo@bar.com")
	assert.Contains(t, restored, "sk-abcdef0123456789abcdef0123456789")
}

// TestExtractSkillPrivacyValues_NoPII verifies that content without
// PII produces an empty values map and unchanged content.
func TestExtractSkillPrivacyValues_NoPII(t *testing.T) {
	content := "name: my-skill\nnotes: nothing secret here"
	res := ExtractSkillPrivacyValues("s", "test", content)
	assert.Empty(t, res.Values)
	assert.Equal(t, content, res.SanitizedContent)
}

// TestGetSkillNameFromURI verifies extraction of skill names from
// viking:// URIs.
func TestGetSkillNameFromURI(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want string
	}{
		{"standard", "viking://user/acme/skills/my-skill/SKILL.md", "my-skill"},
		{"trailing_slash", "viking://user/acme/skills/my-skill/SKILL.md/", "my-skill"},
		{"dashed_name", "viking://user/acme/skills/foo-bar-baz/SKILL.md", "foo-bar-baz"},
		{"no_skill_suffix", "viking://user/acme/skills/my-skill/README.md", ""},
		{"no_skills_marker", "viking://user/acme/files/my-skill/SKILL.md", ""},
		{"nested_path", "viking://user/acme/skills/sub/dir/SKILL.md", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, GetSkillNameFromURI(tc.uri))
		})
	}
}

// TestPlaceholderizeThenRestore_RoundTrip verifies the skill pipeline
// round-trips.
func TestPlaceholderizeThenRestore_RoundTrip(t *testing.T) {
	content := `name: my-skill
user_email: foo@bar.com
api_key: sk-abcdef0123456789abcdef0123456789
`
	res := PlaceholderizeSkillContentWithBlocks(content, "my-skill", map[string]string{
		"email": "foo@bar.com",
		"key":   "sk-abcdef0123456789abcdef0123456789",
	})
	restored := RestoreSkillContent(res.SanitizedContent, "my-skill", res.ReplacedValues)
	assert.Contains(t, restored, "user_email: foo@bar.com")
	assert.Contains(t, restored, "api_key: sk-abcdef0123456789abcdef0123456789")
}
