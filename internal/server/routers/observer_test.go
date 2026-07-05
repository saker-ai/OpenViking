package routers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// newObserverTestRouter builds a gin engine with the observer router mounted.
// It uses a fresh defaultObserverStore per test by calling resetObserverStore
// before constructing the router.
func newObserverTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	resetObserverStore()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterObserver(api, deps)
	return r
}

// newObserverTestDeps returns a minimal Deps with a memfs-backed ragfs.
// The observer store is the package-level singleton; it does not live on
// Deps, so a non-nil Deps is sufficient.
func newObserverTestDeps(t *testing.T) *Deps {
	t.Helper()
	mnt := ragfs.NewMountableFS()
	require.NoError(t, mnt.Mount("/", memfs.New("test")))
	return &Deps{RAGFS: mnt}
}

// resetObserverStore clears the package-level observer singleton so each
// test starts with an empty registry.
func resetObserverStore() {
	defaultObserverStore = newObserverStore()
}

func TestObserver_CreateAndList(t *testing.T) {
	deps := newObserverTestDeps(t)
	r := newObserverTestRouter(t, deps)

	// Create a runtime observer.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/observer", strings.NewReader(`{"kind":"runtime"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var entry observerEntry
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entry))
	assert.Equal(t, "runtime", entry.Kind)
	assert.NotEmpty(t, entry.ID)

	// List observers.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/observer", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listResp struct {
		Observers []*observerEntry `json:"observers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	require.Len(t, listResp.Observers, 1)
	assert.Equal(t, entry.ID, listResp.Observers[0].ID)
}

func TestObserver_Delete(t *testing.T) {
	deps := newObserverTestDeps(t)
	r := newObserverTestRouter(t, deps)

	// Create an observer.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/observer", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var entry observerEntry
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entry))

	// Delete it.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/observer/"+entry.ID, nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Delete again -> 404.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/observer/"+entry.ID, nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestObserver_EventsStream(t *testing.T) {
	deps := newObserverTestDeps(t)
	r := newObserverTestRouter(t, deps)

	// Create a runtime observer.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/observer", strings.NewReader(`{"kind":"runtime"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var entry observerEntry
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entry))

	// Stream events over a real httptest.Server so the SSE goroutine owns
	// the response body. Reading from the live response.Body eliminates the
	// data race that httptest.ResponseRecorder triggers when one goroutine
	// writes the response while another reads Body.String() in a poll loop.
	srv := httptest.NewServer(r)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/v1/observer/"+entry.ID+"/events", nil)
	require.NoError(t, err)
	getReq.Header.Set(identity.HeaderAccount, "acct")

	type readResult struct {
		body string
		err  error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		resp, err := srv.Client().Do(getReq)
		if err != nil {
			resultCh <- readResult{err: err}
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 1024)
		for {
			n, readErr := resp.Body.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				if strings.Contains(string(buf), "event: snapshot") {
					break
				}
			}
			if readErr != nil {
				break
			}
		}
		resultCh <- readResult{body: string(buf)}
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	select {
	case <-deadline.C:
		t.Fatalf("timed out waiting for SSE event")
	case res := <-resultCh:
		require.NoError(t, res.err)
		assert.Contains(t, res.body, "event: snapshot")
		assert.Contains(t, res.body, "goroutine_stacks")
	}
}

func TestObserver_EventsNotFound(t *testing.T) {
	deps := newObserverTestDeps(t)
	r := newObserverTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/observer/missing/events", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestObserver_NilDepsReturns501(t *testing.T) {
	resetObserverStore()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterObserver(api, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/observer", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
