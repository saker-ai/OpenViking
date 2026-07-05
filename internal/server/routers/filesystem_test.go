package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func TestFS_ListAndStat(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	ctx := reqContext(t, r, "acct")

	// Pre-populate the fs with a directory and a file.
	require.NoError(t, deps.RAGFS.Mkdir(ctx, "/accounts/acct/docs", 0o755))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/docs/hello.txt",
		bytes.NewBufferString("hello"), 0o644))

	// ls the directory using the Python-aligned "uri" query param.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fs/ls?uri=/docs", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ls struct {
		Status string             `json:"status"`
		Result []*ragfs.TreeEntry `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ls))
	assert.Equal(t, "ok", ls.Status)
	require.Len(t, ls.Result, 1)
	assert.Equal(t, "hello.txt", ls.Result[0].Info.Name)

	// stat the file using the Python-aligned "uri" query param.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/fs/stat?uri=/docs/hello.txt", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var st struct {
		Status string `json:"status"`
		Result struct {
			Path string `json:"path"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.Equal(t, "ok", st.Status)
	assert.Equal(t, "/accounts/acct/docs/hello.txt", st.Result.Path)
}

// TestFS_ListAcceptsPathAlias verifies the Go-alias "path" query param is
// still accepted (backward compat with Go-authored clients).
func TestFS_ListAcceptsPathAlias(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	ctx := reqContext(t, r, "acct")
	require.NoError(t, deps.RAGFS.Mkdir(ctx, "/accounts/acct/docs", 0o755))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fs/ls?path=/docs", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ls struct {
		Status string             `json:"status"`
		Result []*ragfs.TreeEntry `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ls))
	assert.Equal(t, "ok", ls.Status)
	assert.Empty(t, ls.Result, "empty dir should yield empty entries array")
}

func TestFS_MkdirAndRemove(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)

	// mkdir.
	body := `{"path":"work"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fs/mkdir", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// rm it (recursive=false).
	rmBody := `{"path":"work"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/fs/rm", bytes.NewBufferString(rmBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestFS_MoveAndCopy(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	ctx := reqContext(t, r, "acct")
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/a.txt",
		bytes.NewBufferString("data"), 0o644))

	// copy a.txt -> b.txt
	copyBody := `{"path":"a.txt","to":"b.txt"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fs/copy", bytes.NewBufferString(copyBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// move a.txt -> c.txt
	moveBody := `{"path":"a.txt","to":"c.txt"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/fs/move", bytes.NewBufferString(moveBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// a.txt should be gone, b.txt and c.txt should exist.
	for _, p := range []string{"/accounts/acct/b.txt", "/accounts/acct/c.txt"} {
		info, err := deps.RAGFS.Stat(ctx, p)
		require.NoError(t, err, p)
		require.NotNil(t, info)
	}
	_, err := deps.RAGFS.Stat(ctx, "/accounts/acct/a.txt")
	require.Error(t, err)
}

func TestFS_Grep(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	ctx := reqContext(t, r, "acct")
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/notes.md",
		bytes.NewBufferString("line one\nhello world\nbye"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fs/grep?pattern=hello&path=/notes.md", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Matches []ragfs.GrepMatch `json:"matches"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Matches, 1)
	assert.Equal(t, "hello world", body.Matches[0].Line)
}

func TestFS_Tree(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	ctx := reqContext(t, r, "acct")
	require.NoError(t, deps.RAGFS.Mkdir(ctx, "/accounts/acct/docs", 0o755))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/docs/a.txt",
		bytes.NewBufferString("a"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fs/tree?path=/docs&depth=2", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestFS_GrepValidation(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fs/grep?path=/", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestFS_MoveValidation(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	body := `{"path":"a"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fs/move", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestFS_NilDepsReturnsError(t *testing.T) {
	r := newTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fs/ls?path=/", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
