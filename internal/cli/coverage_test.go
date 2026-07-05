package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClientSetToken verifies SetToken updates the bearer token sent on
// subsequent requests.
func TestClientSetToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	c.SetToken("abc123")
	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, "Bearer abc123", gotAuth)
}

// TestClientDelete verifies Delete issues a DELETE and returns the
// response.
func TestClientDelete(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	resp, err := c.Delete(context.Background(), "/r/foo")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestClientPostForm verifies PostForm sends URL-encoded form data and
// decodes the JSON response.
func TestClientPostForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		assert.Equal(t, "v1", r.FormValue("k1"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"echoed": "v1"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	var out map[string]string
	form := url.Values{"k1": {"v1"}}
	require.NoError(t, c.PostForm(context.Background(), "/post", form, &out))
	assert.Equal(t, "v1", out["echoed"])
}

// TestClientPostForm_NilOut verifies PostForm with nil out discards the
// body without error.
func TestClientPostForm_NilOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	form := url.Values{"k": {"v"}}
	require.NoError(t, c.PostForm(context.Background(), "/post", form, nil))
}

// TestConfigPathAndSaveConfig verifies ConfigPath returns a path under
// HOME/.ov and SaveConfig writes a YAML file there.
func TestConfigPathAndSaveConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path, err := ConfigPath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, ".ov", "config.yaml"), path)

	cfg := &CLIConfig{
		Server:  CLIConfigServer{BaseURL: "http://localhost:9999", Timeout: 5},
		Account: CLIConfigAccount{Name: "testacct"},
		Output:  CLIConfigOutput{Format: "json", Locale: "en"},
	}
	require.NoError(t, SaveConfig(cfg))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(data)
	assert.Contains(t, body, "localhost:9999")
	assert.Contains(t, body, "testacct")
	assert.Contains(t, body, "json")
}

// TestRuntimeLsJSON verifies lsJSON emits valid JSON to stdout.
func TestRuntimeLsJSON(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"name":"a","uri":"viking://testacct/file/a","size":100,"updated_at":"2026-01-01T00:00:00Z"}]}`)
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)
	rt.Config.Output.Format = "json"

	require.NoError(t, rt.lsJSON(context.Background(), "/test", true))
	var parsed []map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &parsed))
	require.Len(t, parsed, 1)
	assert.Equal(t, "a", parsed[0]["name"])
}
