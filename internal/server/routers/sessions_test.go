package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
	"github.com/saker-ai/ctxhub/internal/session"
)

// newSessionsTestRouter builds a gin engine with the sessions router mounted
// and the given Deps injected. Reuses the errorMiddlewareForTest renderer so
// *domain.AppError responses stay consistent with the server middleware.
func newSessionsTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterSessions(api, deps)
	return r
}

// newSessionsTestDeps builds a Deps with an in-memory session store and a
// memfs-backed ragfs so the session router has all its dependencies wired.
func newSessionsTestDeps(t *testing.T) *Deps {
	t.Helper()
	mnt := ragfs.NewMountableFS()
	require.NoError(t, mnt.Mount("/", memfs.New("test")))
	return &Deps{
		RAGFS:    mnt,
		Sessions: session.NewMemoryStore(session.StoreConfig{}),
	}
}

func TestSessions_CreateAndGet(t *testing.T) {
	deps := newSessionsTestDeps(t)
	r := newSessionsTestRouter(t, deps)

	// Create a session. Body uses the Python-aligned session_id field; the
	// Go implementation does not yet honor caller-supplied ids (known gap),
	// but the request must still be accepted and an envelope returned.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{"session_id":"s-123","memory_policy":{"k":"v"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set(identity.HeaderUser, "user1")
	req.Header.Set(identity.HeaderActorPeer, "peer1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created struct {
		Status string `json:"status"`
		Result domain.Session `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, "ok", created.Status)
	assert.Equal(t, "active", string(created.Result.Status))
	assert.Equal(t, "acct", created.Result.Account)
	assert.NotEmpty(t, created.Result.ID)

	// Get the session.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+created.Result.ID, nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set(identity.HeaderUser, "user1")
	req.Header.Set(identity.HeaderActorPeer, "peer1")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var fetched struct {
		Status string `json:"status"`
		Result domain.Session `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fetched))
	assert.Equal(t, "ok", fetched.Status)
	assert.Equal(t, created.Result.ID, fetched.Result.ID)
}

func TestSessions_AppendTurnAndCommit(t *testing.T) {
	deps := newSessionsTestDeps(t)
	r := newSessionsTestRouter(t, deps)

	// Create a session.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		Status string `json:"status"`
		Result domain.Session `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// Append a turn.
	body := `{"role":"user","content":"hello","tokens":10}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.Result.ID+"/turns", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Commit the session.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.Result.ID+"/commit", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var committed struct {
		Status string `json:"status"`
		Result domain.Session `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &committed))
	assert.Equal(t, "ok", committed.Status)
	assert.Equal(t, "committed", string(committed.Result.Status))
}

func TestSessions_ListAndArchive(t *testing.T) {
	deps := newSessionsTestDeps(t)
	r := newSessionsTestRouter(t, deps)

	// Create two sessions.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(identity.HeaderAccount, "acct")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	// List sessions.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listResp struct {
		Status   string            `json:"status"`
		Result   []*domain.Session `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	assert.Equal(t, "ok", listResp.Status)
	assert.Len(t, listResp.Result, 2)

	// Archive the first one.
	sid := listResp.Result[0].ID
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sid+"/archive", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Verify the archived status.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sid, nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var fetched struct {
		Status string `json:"status"`
		Result domain.Session `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fetched))
	assert.Equal(t, "ok", fetched.Status)
	assert.Equal(t, "archived", string(fetched.Result.Status))
}

func TestSessions_MemoryApplyAndList(t *testing.T) {
	deps := newSessionsTestDeps(t)
	r := newSessionsTestRouter(t, deps)

	// Create a session.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		Status string `json:"status"`
		Result domain.Session `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, "ok", created.Status)

	// Apply a memory diff (add one fact).
	diffBody := `{"added":[{"type":"fact","content":"user likes go","confidence":0.9}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.Result.ID+"/memory", bytes.NewBufferString(diffBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// List memory.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+created.Result.ID+"/memory", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var memResp struct {
		Status   string `json:"status"`
		Result   struct {
			Memory []domain.ExtractedMemory `json:"memory"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &memResp))
	require.Equal(t, "ok", memResp.Status)
	require.Len(t, memResp.Result.Memory, 1)
	assert.Equal(t, "user likes go", memResp.Result.Memory[0].Content)
}

func TestSessions_Compact(t *testing.T) {
	deps := newSessionsTestDeps(t)
	r := newSessionsTestRouter(t, deps)

	// Create a session.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		Status string `json:"status"`
		Result domain.Session `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, "ok", created.Status)

	// Compact the session (runs Commit, which is idempotent on a fresh
	// session with no compressor configured).
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.Result.ID+"/compact", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		SessionID string `json:"session_id"`
		Compacted bool   `json:"compacted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, created.Result.ID, resp.SessionID)
	assert.True(t, resp.Compacted)
}

func TestSessions_ExportSkill(t *testing.T) {
	deps := newSessionsTestDeps(t)
	r := newSessionsTestRouter(t, deps)

	// Create a session.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		Status string `json:"status"`
		Result domain.Session `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, "ok", created.Status)

	// Add a skill and a fact to memory.
	diffBody := `{"added":[{"type":"skill","content":"can write go"},{"type":"fact","content":"likes go"}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.Result.ID+"/memory", bytes.NewBufferString(diffBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Export skills — should return only the skill-typed memory.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.Result.ID+"/export-skill", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Skills []domain.ExtractedMemory `json:"skills"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Skills, 1)
	assert.Equal(t, "can write go", resp.Skills[0].Content)
}

func TestSessions_NilDepsReturns501(t *testing.T) {
	r := newSessionsTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestSessions_MissingIdentityReturns401(t *testing.T) {
	deps := newSessionsTestDeps(t)
	r := newSessionsTestRouter(t, deps)
	// No X-OpenViking-Account header -> identity middleware rejects with 401.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// Ensure context is referenced for future tests that propagate identity
// through the request context directly.
var _ = context.Background
