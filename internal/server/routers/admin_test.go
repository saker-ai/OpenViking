package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func newAdminTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterAdmin(api, deps)
	return r
}

// adminReq is a tiny helper that builds an *http.Request with the admin
// account header pre-set so the identity middleware doesn't reject the
// call. Admin endpoints are cross-account but still sit under /api/v1.
func adminReq(method, url string, body *bytes.Buffer) *http.Request {
	var r *bytes.Buffer
	if body != nil {
		r = body
	} else {
		r = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, url, r)
	req.Header.Set(identity.HeaderAccount, "admin")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestAdmin_AccountsListEmpty(t *testing.T) {
	deps := newTestDeps(t)
	r := newAdminTestRouter(t, deps)
	req := adminReq(http.MethodGet, "/api/v1/admin/accounts", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Status  string `json:"status"`
		Result struct {
			Accounts []string `json:"accounts"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ok", body.Status)
	assert.Empty(t, body.Result.Accounts)
}

func TestAdmin_AccountCreateGetDelete(t *testing.T) {
	deps := newTestDeps(t)
	r := newAdminTestRouter(t, deps)

	// create with the Go-alias id field (still accepted)
	body := `{"id":"acme","name":"Acme Inc.","metadata":{"tier":"pro"}}`
	req := adminReq(http.MethodPost, "/api/v1/admin/accounts", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createResp struct {
		Status string `json:"status"`
		Result struct {
			AccountID string `json:"account_id"`
			Account   struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"account"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	assert.Equal(t, "ok", createResp.Status)
	assert.Equal(t, "acme", createResp.Result.AccountID)
	assert.Equal(t, "acme", createResp.Result.Account.ID)
	assert.Equal(t, "active", createResp.Result.Account.State)

	// get
	req = adminReq(http.MethodGet, "/api/v1/admin/accounts/acme", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got struct {
		Status string `json:"status"`
		Result struct {
			Account struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"account"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "ok", got.Status)
	assert.Equal(t, "acme", got.Result.Account.ID)
	assert.Equal(t, "active", got.Result.Account.State)

	// delete
	req = adminReq(http.MethodDelete, "/api/v1/admin/accounts/acme", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var delResp struct {
		Status string `json:"status"`
		Result struct {
			Deleted bool `json:"deleted"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &delResp))
	assert.Equal(t, "ok", delResp.Status)
	assert.True(t, delResp.Result.Deleted)

	// get -> 404
	req = adminReq(http.MethodGet, "/api/v1/admin/accounts/acme", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestAdmin_AccountCreateWithPythonFields verifies the Python-aligned
// CreateAccountRequest fields (account_id, admin_user_id, seed) are
// accepted and reflected in the response result.
func TestAdmin_AccountCreateWithPythonFields(t *testing.T) {
	deps := newTestDeps(t)
	r := newAdminTestRouter(t, deps)
	body := `{"account_id":"beta","admin_user_id":"admin1","seed":"seed-xyz"}`
	req := adminReq(http.MethodPost, "/api/v1/admin/accounts", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp struct {
		Status string `json:"status"`
		Result struct {
			AccountID   string `json:"account_id"`
			AdminUserID string `json:"admin_user_id"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, "beta", resp.Result.AccountID)
	assert.Equal(t, "admin1", resp.Result.AdminUserID)
}

func TestAdmin_AccountCreateConflict(t *testing.T) {
	deps := newTestDeps(t)
	r := newAdminTestRouter(t, deps)

	body := `{"id":"dup"}`
	req := adminReq(http.MethodPost, "/api/v1/admin/accounts", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	req = adminReq(http.MethodPost, "/api/v1/admin/accounts", bytes.NewBufferString(body))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestAdmin_APIKeyCreateAndRevoke(t *testing.T) {
	deps := newTestDeps(t)
	r := newAdminTestRouter(t, deps)

	// create account first
	req := adminReq(http.MethodPost, "/api/v1/admin/accounts", bytes.NewBufferString(`{"id":"acme"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// create api key — server generates the plaintext; key_id in the
	// response is a uuid, returned once alongside the plaintext key.
	req = adminReq(http.MethodPost, "/api/v1/admin/accounts/acme/api-keys",
		bytes.NewBufferString(`{"name":"k1","scope":"read"}`))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createBody struct {
		KeyID string `json:"key_id"`
		Key   string `json:"key"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createBody))
	require.NotEmpty(t, createBody.KeyID, "key_id (uuid) must be returned")
	require.NotEmpty(t, createBody.Key, "plaintext key must be returned once")

	// soft-revoke via the uuid returned by create.
	req = adminReq(http.MethodDelete, "/api/v1/admin/accounts/acme/api-keys/"+createBody.KeyID, nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Revoked bool `json:"revoked"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body.Revoked)
}

// TestAdmin_APIKeyAcceptsLegacyKeyID ensures the request body field
// key_id (legacy, client-supplied) is still accepted as the name when
// name is empty, preserving backward compatibility with old clients.
func TestAdmin_APIKeyAcceptsLegacyKeyID(t *testing.T) {
	deps := newTestDeps(t)
	r := newAdminTestRouter(t, deps)

	req := adminReq(http.MethodPost, "/api/v1/admin/accounts", bytes.NewBufferString(`{"id":"acme"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	req = adminReq(http.MethodPost, "/api/v1/admin/accounts/acme/api-keys",
		bytes.NewBufferString(`{"key_id":"legacy-name","scope":"read"}`))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createBody struct {
		APIKey struct {
			Name string `json:"name"`
		} `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createBody))
	assert.Equal(t, "legacy-name", createBody.APIKey.Name)
}

func TestAdmin_APIKeyForMissingAccount(t *testing.T) {
	deps := newTestDeps(t)
	r := newAdminTestRouter(t, deps)
	req := adminReq(http.MethodPost, "/api/v1/admin/accounts/ghost/api-keys",
		bytes.NewBufferString(`{"key_id":"k1"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdmin_UsersListEmpty(t *testing.T) {
	deps := newTestDeps(t)
	r := newAdminTestRouter(t, deps)
	req := adminReq(http.MethodGet, "/api/v1/admin/users", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Users []string `json:"users"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Empty(t, body.Users)
}

func TestAdmin_UserCreateAndDelete(t *testing.T) {
	deps := newTestDeps(t)
	r := newAdminTestRouter(t, deps)

	req := adminReq(http.MethodPost, "/api/v1/admin/users",
		bytes.NewBufferString(`{"id":"u1","account":"acme"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// list
	req = adminReq(http.MethodGet, "/api/v1/admin/users", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Users []string `json:"users"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body.Users, "u1")

	// delete
	req = adminReq(http.MethodDelete, "/api/v1/admin/users/u1", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAdmin_NilDepsReturnsError(t *testing.T) {
	r := newAdminTestRouter(t, &Deps{})
	req := adminReq(http.MethodGet, "/api/v1/admin/accounts", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
