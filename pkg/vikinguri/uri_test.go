package vikinguri

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAgentRoot(t *testing.T) {
	u, err := Parse("viking://agent/skills")
	require.NoError(t, err)
	assert.True(t, u.Scope.IsAgent())
	assert.Equal(t, KindSkills, u.Kind)
	assert.Equal(t, "", u.Path)
}

func TestParseUserScoped(t *testing.T) {
	u, err := Parse("viking://user_42/sessions/sess_abc")
	require.NoError(t, err)
	assert.Equal(t, "user", u.Scope.Type)
	assert.Equal(t, "42", u.Scope.UserID)
	assert.Equal(t, KindSessions, u.Kind)
	assert.Equal(t, "/sess_abc", u.Path)
}

func TestParseResourcesNested(t *testing.T) {
	u, err := Parse("viking://agent/resources/docs/intro.md")
	require.NoError(t, err)
	assert.Equal(t, KindResources, u.Kind)
	assert.Equal(t, "/docs/intro.md", u.Path)
}

func TestParseQuery(t *testing.T) {
	u, err := Parse("viking://agent/resources/docs/intro.md?layer=abstract&v=2")
	require.NoError(t, err)
	assert.Equal(t, "abstract", u.Query.Get("layer"))
	assert.Equal(t, "2", u.Query.Get("v"))
}

func TestParseInvalidScheme(t *testing.T) {
	_, err := Parse("https://agent/skills")
	require.Error(t, err)
}

func TestParseMissingScope(t *testing.T) {
	_, err := Parse("viking:///skills")
	require.Error(t, err)
}

func TestParseUnknownKind(t *testing.T) {
	_, err := Parse("viking://agent/bogus")
	require.Error(t, err)
}

func TestRoundTrip(t *testing.T) {
	cases := []string{
		"viking://agent/skills",
		"viking://agent/resources/docs/intro.md",
		"viking://user_42/sessions/sess_abc",
	}
	for _, s := range cases {
		u, err := Parse(s)
		require.NoError(t, err)
		assert.Equal(t, s, u.String())
	}
}

func TestNormalize(t *testing.T) {
	got, err := Normalize("viking://agent/resources/docs/intro.md")
	require.NoError(t, err)
	assert.Equal(t, "viking://agent/resources/docs/intro.md", got)
}

func TestScopeString(t *testing.T) {
	assert.Equal(t, "agent", Scope{Type: "agent"}.String())
	assert.Equal(t, "user_42", Scope{Type: "user", UserID: "42"}.String())
}
