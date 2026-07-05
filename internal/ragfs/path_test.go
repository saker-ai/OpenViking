package ragfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"docs", "/docs"},
		{"/docs/", "/docs/"},
		{"/docs//intro.md", "/docs/intro.md"},
		{"docs/../docs/intro.md", "/docs/intro.md"},
		{"/", "/"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, Normalize(c.in), "input %q", c.in)
	}
}

func TestJoin(t *testing.T) {
	assert.Equal(t, "/docs/intro.md", Join("/docs", "intro.md"))
	assert.Equal(t, "/docs/sub/x.md", Join("/docs", "sub", "x.md"))
	assert.Equal(t, "/", Join())
}

func TestDirBase(t *testing.T) {
	assert.Equal(t, "/docs", Dir("/docs/intro.md"))
	assert.Equal(t, "/", Dir("/docs"))
	assert.Equal(t, "intro.md", Base("/docs/intro.md"))
	assert.Equal(t, "", Base("/"))
}

func TestHiddenSidecar(t *testing.T) {
	// file resource: sidecar is sibling with suffix appended
	assert.Equal(t, "/docs/intro.md.abstract",
		HiddenSidecar("/docs/intro.md", ".abstract"))
	// directory resource: sidecar lives inside
	assert.Equal(t, "/docs/.abstract",
		HiddenSidecar("/docs/", ".abstract"))
	// root directory
	assert.Equal(t, "/.abstract", HiddenSidecar("/", ".abstract"))
}

func TestIsHidden(t *testing.T) {
	assert.True(t, IsHidden(".abstract"))
	assert.False(t, IsHidden("intro.md"))
}

func TestIsNotFoundIsConflict(t *testing.T) {
	// ErrUnsupported is wrapped with the ragfs code, not NotFound/Conflict.
	assert.False(t, IsNotFound(ErrUnsupported))
	assert.False(t, IsConflict(ErrUnsupported))
}
