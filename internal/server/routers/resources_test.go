package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/auth/apikeys"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// newTestRouter builds a gin engine with the resources router mounted and
// the given Deps injected. The identity middleware sets the account context
// from the X-OpenViking-Account header; an in-package error renderer
// mimics server.errorMiddleware so *domain.AppError is rendered as a
// structured envelope without importing the server package (which would
// create a circular dependency).
func newTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterResources(api, deps)
	RegisterContent(api, deps)
	RegisterFilesystem(api, deps)
	RegisterConsole(api, deps)
	return r
}

// errorMiddlewareForTest renders *domain.AppError as a structured envelope
// {error:{code,message,details}} with the matching HTTP status. Non-AppError
// errors render as 500 INTERNAL_ERROR. This mirrors server.errorMiddleware
// without dragging the server package into the routers package (which would
// be a circular import).
func errorMiddlewareForTest() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if len(c.Errors) == 0 {
			return
		}
		first := c.Errors[0].Err
		var appErr *domain.AppError
		status := http.StatusInternalServerError
		code := domain.CodeInternalError
		msg := first.Error()
		if ae, ok := first.(*domain.AppError); ok {
			appErr = ae
			_ = appErr
			if ae.Status != 0 {
				status = ae.Status
			}
			code = ae.Code
			if ae.Err != nil {
				msg = ae.Err.Error()
			}
		}
		c.JSON(status, gin.H{
			"error": gin.H{
				"code":    code,
				"message": msg,
			},
		})
	}
}

// newTestDeps builds a Deps with a memfs-backed MountableFS mounted at "/"
// and an apikeys.Manager backed by a per-test temp file.
func newTestDeps(t *testing.T) *Deps {
	t.Helper()
	mnt := ragfs.NewMountableFS()
	require.NoError(t, mnt.Mount("/", memfs.New("test")))
	apiKeys, err := apikeys.NewManager(filepath.Join(t.TempDir(), "apikeys.json"))
	require.NoError(t, err)
	return &Deps{RAGFS: mnt, APIKeys: apiKeys}
}

// reqContext returns a context carrying the test identity so handlers and
// helpers that read identity.FromContext see the same account as the
// X-OpenViking-Account header.
func reqContext(t *testing.T, r *gin.Engine, account string) context.Context {
	t.Helper()
	id := domain.Identifier{Account: account}
	return identity.WithIdentity(context.Background(), id)
}

func TestResources_ListEmpty(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/resources", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Entries []*ragfs.TreeEntry `json:"entries"`
		Path    string            `json:"path"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	// Resources root is /accounts/acct/resources; entries may be empty
	// because the memfs has nothing there yet.
	assert.Equal(t, "/accounts/acct/resources", body.Path)
}

func TestResources_CreateAndGet(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)

	// Create a directory resource.
	body := `{"path":"notes","is_dir":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/resources", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Stat the created directory.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/resources/notes", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var stat struct {
		Path string           `json:"path"`
		Info *ragfs.FileInfo `json:"info"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stat))
	assert.Equal(t, "/accounts/acct/resources/notes", stat.Path)
	if assert.NotNil(t, stat.Info) {
		assert.True(t, stat.Info.IsDir)
	}
}

func TestResources_PutAndDelete(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)

	// PUT a file.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/resources/hello.txt",
		bytes.NewBufferString("hello world"))
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// DELETE it.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/resources/hello.txt", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// GET should now fail (not found -> RAGFS error wrapped -> 500).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/resources/hello.txt", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestResources_Head(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)

	// PUT a file first.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/resources/hello.txt",
		bytes.NewBufferString("hello world"))
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// HEAD it.
	req = httptest.NewRequest(http.MethodHead, "/api/v1/resources/hello.txt", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("X-Resource-Name"))
}

func TestResources_CreateValidation(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	// Empty body should fail validation (path required).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/resources", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestResources_NilDepsReturnsError(t *testing.T) {
	r := newTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/resources", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

