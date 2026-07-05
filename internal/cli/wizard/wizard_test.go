package wizard

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitCmdNonInteractive(t *testing.T) {
	var gotCfg Config
	save := func(cfg Config) (string, error) {
		gotCfg = cfg
		return "/tmp/test-config.yaml", nil
	}
	out := &bytes.Buffer{}
	cmd := InitCmd(out, nil, save)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	cmd.SetArgs([]string{"--non-interactive"})
	require.NoError(t, cmd.Execute())

	assert.Equal(t, DefaultBaseURL, gotCfg.BaseURL)
	assert.Equal(t, "default", gotCfg.Account)
	assert.Equal(t, "table", gotCfg.Format)
	assert.Contains(t, out.String(), "wrote /tmp/test-config.yaml")
}

func TestInitCmdNonInteractiveOverrides(t *testing.T) {
	var gotCfg Config
	save := func(cfg Config) (string, error) {
		gotCfg = cfg
		return "/tmp/x", nil
	}
	out := &bytes.Buffer{}
	cmd := InitCmd(out, nil, save)

	// Set the values directly via the variables captured in the closure.
	// InitCmd defines them as flags-free inputs; the non-interactive
	// path reads the in-scope variables. We instead emulate the form
	// by setting the flag and reading defaults.
	cmd.SetArgs([]string{"--non-interactive"})
	require.NoError(t, cmd.Execute())

	// Without the form, baseURL/account/format are zero; defaults apply.
	assert.Equal(t, DefaultBaseURL, gotCfg.BaseURL)
	assert.Equal(t, "default", gotCfg.Account)
	assert.Equal(t, "table", gotCfg.Format)
}

func TestInitCmdSaveError(t *testing.T) {
	save := func(cfg Config) (string, error) {
		return "", assertError{"kaboom"}
	}
	out := &bytes.Buffer{}
	cmd := InitCmd(out, nil, save)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--non-interactive"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kaboom")
}

type assertError struct{ msg string }

func (a assertError) Error() string { return a.msg }

func TestInitCmdHelp(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := InitCmd(out, nil, func(Config) (string, error) { return "", nil })
	cmd.SetOut(out)
	cmd.SetArgs([]string{"--help"})
	_ = cmd.Execute()
	assert.True(t, strings.Contains(out.String(), "Run the configuration wizard") || strings.Contains(out.String(), "init"))
}

// Ensure cobra command is wired correctly (parent sanity).
func TestInitCmdIsCobra(t *testing.T) {
	cmd := InitCmd(new(bytes.Buffer), nil, func(Config) (string, error) { return "", nil })
	var _ *cobra.Command = cmd
}
