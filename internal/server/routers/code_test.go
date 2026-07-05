package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func TestCode_OutlineGo(t *testing.T) {
	deps := newTestDeps(t)
	ctx := reqContext(t, nil, "acct")
	src := []byte("package main\n\nimport \"fmt\"\n\nfunc hello() {\n\tfmt.Println(\"hi\")\n}\n\ntype Foo struct{ Bar int }\n")
	require.NoError(t, deps.RAGFS.Mkdir(ctx, "/accounts/acct/code", 0o755))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/code/main.go", bytes.NewReader(src), 0o644))
	r := newTestRouterAll(t, deps)

	// Use the Python-aligned "uri" field; "path" alias still accepted.
	body := `{"uri":"/main.go"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/code/outline", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Status string `json:"status"`
		Result struct {
			Path    string         `json:"path"`
			Outline []outlineEntry `json:"outline"`
			Size    int            `json:"size"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, "/accounts/acct/code/main.go", resp.Result.Path)
	assert.Greater(t, resp.Result.Size, 0)
	// Expect at least one func and one type entry.
	var sawFunc, sawType bool
	for _, e := range resp.Result.Outline {
		if e.Kind == "func" && e.Name == "hello" {
			sawFunc = true
		}
		if e.Kind == "type" && e.Name == "Foo" {
			sawType = true
		}
	}
	assert.True(t, sawFunc, "should detect hello func, got %v", resp.Result.Outline)
	assert.True(t, sawType, "should detect Foo type, got %v", resp.Result.Outline)
}

func TestCode_Search(t *testing.T) {
	deps := newTestDeps(t)
	ctx := reqContext(t, nil, "acct")
	require.NoError(t, deps.RAGFS.Mkdir(ctx, "/accounts/acct/code", 0o755))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/code/a.go",
		bytes.NewReader([]byte("package main\nfunc foo() {}\n")), 0o644))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/code/b.go",
		bytes.NewReader([]byte("package main\nfunc bar() {}\n")), 0o644))
	r := newTestRouterAll(t, deps)

	// Use the Python-aligned "uri" + "query" fields.
	body := `{"uri":"/","query":"func","recursive":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/code/search", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Status string `json:"status"`
		Result struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
			Matches []any  `json:"matches"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, "func", resp.Result.Pattern)
	assert.GreaterOrEqual(t, len(resp.Result.Matches), 2)
}

func TestCode_Expand(t *testing.T) {
	deps := newTestDeps(t)
	ctx := reqContext(t, nil, "acct")
	src := []byte("line1\nline2\nline3\nline4\nline5\n")
	require.NoError(t, deps.RAGFS.Mkdir(ctx, "/accounts/acct/code", 0o755))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/code/file.txt", bytes.NewReader(src), 0o644))
	r := newTestRouterAll(t, deps)

	body := `{"uri":"/file.txt","start":2,"end":4}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/code/expand", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Status string `json:"status"`
		Result struct {
			Start int      `json:"start"`
			End   int      `json:"end"`
			Total int      `json:"total"`
			Lines []string `json:"lines"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, 2, resp.Result.Start)
	assert.Equal(t, 4, resp.Result.End)
	assert.Equal(t, 5, resp.Result.Total)
	require.Len(t, resp.Result.Lines, 3)
	assert.Equal(t, "line2", resp.Result.Lines[0])
	assert.Equal(t, "line4", resp.Result.Lines[2])
}

// TestCode_OutlineAcceptsPathAlias verifies the Go-alias "path" field is
// still accepted (backward compat with Go-authored clients).
func TestCode_OutlineAcceptsPathAlias(t *testing.T) {
	deps := newTestDeps(t)
	ctx := reqContext(t, nil, "acct")
	src := []byte("package main\nfunc foo() {}\n")
	require.NoError(t, deps.RAGFS.Mkdir(ctx, "/accounts/acct/code", 0o755))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/code/main.go", bytes.NewReader(src), 0o644))
	r := newTestRouterAll(t, deps)

	body := `{"path":"/main.go"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/code/outline", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Status string `json:"status"`
		Result struct {
			Size int `json:"size"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.Greater(t, resp.Result.Size, 0)
}

func TestCode_OutlineValidation(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	// Missing path -> 422.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/code/outline",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestCode_SearchMissingFile(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	body := `{"path":"/missing","pattern":"foo"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/code/search", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// Grep on a missing path returns a ragfs error -> 500 envelope.
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestCode_NilDepsReturns501(t *testing.T) {
	r := newTestRouterAll(t, &Deps{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/code/outline",
		bytes.NewBufferString(`{"path":"/x.go"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
