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

	"github.com/saker-ai/ctxhub/internal/queuefs"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// newTasksTestRouter builds a gin engine with the tasks router mounted.
func newTasksTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterTasks(api, deps)
	return r
}

// newTasksTestDeps builds a Deps with a memfs-backed ragfs and a started
// memory queue server. The queue is started so Enqueue succeeds; a no-op
// handler is registered so workers drain tasks without erroring.
func newTasksTestDeps(t *testing.T) *Deps {
	t.Helper()
	mnt := ragfs.NewMountableFS()
	require.NoError(t, mnt.Mount("/", memfs.New("test")))
	q := queuefs.NewMemoryServer()
	q.RegisterHandler("parse", func(_ context.Context, _ *queuefs.Task) (*queuefs.TaskResult, error) {
		return &queuefs.TaskResult{}, nil
	})
	require.NoError(t, q.Start(context.Background()))
	t.Cleanup(func() { _ = q.Shutdown() })
	return &Deps{RAGFS: mnt, Queue: q}
}

func TestTasks_EnqueueAndGet(t *testing.T) {
	deps := newTasksTestDeps(t)
	r := newTasksTestRouter(t, deps)

	body := `{"id":"task-1","type":"parse","payload":"aGVsbG8=","priority":2}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var ts TaskStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ts))
	assert.Equal(t, "task-1", ts.ID)
	assert.Equal(t, "parse", ts.Type)
	assert.Equal(t, "queued", ts.State)

	// Get the task.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-1", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ts))
	assert.Equal(t, "task-1", ts.ID)
}

func TestTasks_ListEmpty(t *testing.T) {
	deps := newTasksTestDeps(t)
	r := newTasksTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Tasks []*TaskStatus `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Tasks)
}

func TestTasks_CancelAndRetry(t *testing.T) {
	deps := newTasksTestDeps(t)
	r := newTasksTestRouter(t, deps)

	// Enqueue a task.
	body := `{"id":"task-2","type":"parse"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Cancel it.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/task-2/cancel", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ts TaskStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ts))
	assert.Equal(t, "canceled", ts.State)

	// Retry it.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/task-2/retry", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ts))
	assert.Equal(t, "queued", ts.State)
}

func TestTasks_ResultNotFinished(t *testing.T) {
	deps := newTasksTestDeps(t)
	r := newTasksTestRouter(t, deps)

	// Enqueue a task.
	body := `{"id":"task-3","type":"parse"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Result should return 409 (not finished).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-3/result", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestTasks_NotFound(t *testing.T) {
	deps := newTasksTestDeps(t)
	r := newTasksTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/missing", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestTasks_NilDepsReturns501(t *testing.T) {
	r := newTasksTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
