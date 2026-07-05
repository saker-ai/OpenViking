package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRootVersionCmd(t *testing.T) {
	root := NewRoot()
	out := &bytes.Buffer{}
	root.SetOut(out)
	require.NoError(t, Execute(root, []string{"version"}))
	assert.True(t, strings.Contains(out.String(), "openviking"))
}

func TestNewRootUnknownCmd(t *testing.T) {
	root := NewRoot()
	err := Execute(root, []string{"no-such-subcommand"})
	require.Error(t, err)
}
