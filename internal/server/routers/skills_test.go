package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func TestSkills_CreateAndGet(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	body := `{"name":"greet","description":"say hello","trigger":"hi","steps":[{"order":1,"action":"print","description":"print greeting"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created struct {
		Result struct {
			Skill *domain.Skill `json:"skill"`
			Path  string        `json:"path"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.Result.Skill)
	assert.Equal(t, "greet", created.Result.Skill.Name)
	assert.Equal(t, "/accounts/acct/skills/greet.json", created.Result.Path)

	// GET it back.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/greet", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var fetched struct {
		Result *domain.Skill `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fetched))
	require.NotNil(t, fetched.Result)
	assert.Equal(t, "greet", fetched.Result.Name)
	assert.Equal(t, "say hello", fetched.Result.Description)
	require.Len(t, fetched.Result.Steps, 1)
}

func TestSkills_List(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	// Create two skills.
	for _, name := range []string{"alpha", "beta"} {
		body := `{"name":"` + name + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/skills", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(identity.HeaderAccount, "acct")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	// List should show both.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Result struct {
			Skills []map[string]any `json:"skills"`
			Total  int              `json:"total"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Result.Total)
	assert.Len(t, resp.Result.Skills, 2)
	names := []string{resp.Result.Skills[0]["name"].(string), resp.Result.Skills[1]["name"].(string)}
	assert.ElementsMatch(t, []string{"alpha", "beta"}, names)
}

func TestSkills_ListEmpty(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Result struct {
			Skills []any `json:"skills"`
			Total  int   `json:"total"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Result.Skills)
	assert.Equal(t, 0, resp.Result.Total)
}

func TestSkills_Update(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	// Create.
	body := `{"name":"greet","description":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Update.
	body = `{"name":"greet","description":"hello world"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/skills/greet", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var updated struct {
		Result struct {
			Skill *domain.Skill `json:"skill"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.NotNil(t, updated.Result.Skill)
	assert.Equal(t, "hello world", updated.Result.Skill.Description)
}

func TestSkills_Delete(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	// Create.
	body := `{"name":"greet"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Delete.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/skills/greet", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// GET should 404.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/greet", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSkills_CreateConflict(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	body := `{"name":"greet"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Second create with same name should conflict.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/skills", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestSkills_NilDepsReturns501(t *testing.T) {
	r := newTestRouterAll(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
