package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer returns an httptest.Server that replies to the
// registered handler. The server's URL is used to build a Client via
// newTestRuntime.
func newTestServer(t *testing.T, fn func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(fn))
}

// newTestRuntime builds a Runtime whose Client points at srv, with
// stdout/stderr as buffers and a default config.
func newTestRuntime(t *testing.T, srv *httptest.Server) (*Runtime, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cfg := &CLIConfig{
		Server:  CLIConfigServer{BaseURL: srv.URL, Timeout: 5},
		Account: CLIConfigAccount{Name: "test"},
		Output:  CLIConfigOutput{Format: "table", Locale: "en"},
	}
	out := &bytes.Buffer{}
	err := &bytes.Buffer{}
	rt := &Runtime{
		Config: cfg,
		Out:    out,
		Err:    err,
		Client: NewClient(srv.URL, WithAccount("test")),
		Locale: "en",
	}
	return rt, out, err
}

// newTestRuntimeNoServer builds a Runtime with no HTTP server. Used by
// tests that exercise local-only logic (e.g. crypto).
func newTestRuntimeNoServer(t *testing.T) (*Runtime, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cfg := &CLIConfig{
		Account: CLIConfigAccount{Name: "test"},
		Output:  CLIConfigOutput{Format: "table", Locale: "en"},
	}
	out := &bytes.Buffer{}
	err := &bytes.Buffer{}
	rt := &Runtime{
		Config: cfg,
		Out:    out,
		Err:    err,
		Locale: "en",
	}
	return rt, out, err
}

func TestAddResourceCmd(t *testing.T) {
	var gotBody map[string]any
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/resources", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "test", r.Header.Get("X-OpenViking-Account"))
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uri":"viking://test/files/x"}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(AddResourceCmd(rt))
	require.NoError(t, Execute(cmd, []string{"/tmp/x", "--type=file"}))
	assert.Equal(t, "file", gotBody["type"])
	assert.Equal(t, "/tmp/x", gotBody["path"])
	assert.Contains(t, out.String(), "Resource added: /tmp/x")
	assert.Contains(t, out.String(), "viking://test/files/x")
}

func TestLsCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/fs/ls", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"uri":"a","type":"file","name":"a","size":10,"modified_at":"2024-01-01T00:00:00Z"}]}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(LsCmd(rt))
	require.NoError(t, Execute(cmd, []string{}))
	assert.Contains(t, out.String(), "a")
}

func TestLsCmdLong(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"uri":"a","type":"file","name":"a","size":10,"modified_at":"2024-01-01T00:00:00Z"}]}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(LsCmd(rt))
	require.NoError(t, Execute(cmd, []string{"--long"}))
	assert.Contains(t, out.String(), "TYPE")
	assert.Contains(t, out.String(), "file")
}

func TestReadCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/content/a", r.URL.Path)
		_, _ = w.Write([]byte("hello world"))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(ReadCmd(rt))
	require.NoError(t, Execute(cmd, []string{"a"}))
	assert.Equal(t, "hello world", out.String())
}

func TestReadCmdAppError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"RESOURCE_NOT_FOUND","message":"missing"}}`))
	})
	defer srv.Close()
	rt, _, _ := newTestRuntime(t, srv)

	cmd := silence(ReadCmd(rt))
	err := Execute(cmd, []string{"missing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RESOURCE_NOT_FOUND")
}

// silence suppresses cobra's stderr error/usage printing in tests.
func silence(cmd *cobra.Command) *cobra.Command {
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	return cmd
}
