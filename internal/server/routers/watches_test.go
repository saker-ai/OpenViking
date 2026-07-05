package routers

import (
	"bytes"
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

// newWatchesTestRouter builds a gin engine with the watches router mounted.
func newWatchesTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterWatches(api, deps)
	return r
}

// newWatchesTestDeps returns a Deps with a memfs-backed ragfs.
func newWatchesTestDeps(t *testing.T) *Deps {
	t.Helper()
	mnt := ragfs.NewMountableFS()
	require.NoError(t, mnt.Mount("/", memfs.New("test")))
	return &Deps{RAGFS: mnt}
}

// seedWatchedFile writes a file under the caller's account so the watch
// target exists.
func seedWatchedFile(t *testing.T, deps *Deps, account, rel, content string) {
	t.Helper()
	p := ragfs.Normalize("/accounts/" + account + "/" + rel)
	require.NoError(t, deps.RAGFS.Write(context.Background(), p, bytes.NewReader([]byte(content)), 0o644))
}

func TestWatches_CreateAndList(t *testing.T) {
	deps := newWatchesTestDeps(t)
	r := newWatchesTestRouter(t, deps)
	seedWatchedFile(t, deps, "acct", "docs/note.txt", "hello")

	body := `{"path":"docs/note.txt"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/watches", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var create struct {
		Status string     `json:"status"`
		Result watchEntry `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &create))
	assert.Equal(t, "ok", create.Status)
	w := create.Result
	assert.NotEmpty(t, w.ID)
	assert.Contains(t, w.Path, "docs/note.txt")
	require.NotNil(t, w.Snapshot)
	assert.False(t, w.Snapshot.IsDir)

	// List watches.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/watches", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listResp struct {
		Status string `json:"status"`
		Result struct {
			Tasks  []*watchEntry `json:"tasks"`
			Total  int           `json:"total"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	assert.Equal(t, "ok", listResp.Status)
	require.Len(t, listResp.Result.Tasks, 1)
	assert.Equal(t, w.ID, listResp.Result.Tasks[0].ID)
}

func TestWatches_CreateMissingTarget(t *testing.T) {
	deps := newWatchesTestDeps(t)
	r := newWatchesTestRouter(t, deps)

	body := `{"path":"does/not/exist.txt"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/watches", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWatches_Delete(t *testing.T) {
	deps := newWatchesTestDeps(t)
	r := newWatchesTestRouter(t, deps)
	seedWatchedFile(t, deps, "acct", "docs/note.txt", "hello")

	// Create a watch.
	body := `{"path":"docs/note.txt"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/watches", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var create struct {
		Status string     `json:"status"`
		Result watchEntry `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &create))
	w := create.Result

	// Delete it.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/watches/"+w.ID, nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Delete again -> 404.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/watches/"+w.ID, nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWatches_EventsChangeDetected(t *testing.T) {
	deps := newWatchesTestDeps(t)
	r := newWatchesTestRouter(t, deps)
	seedWatchedFile(t, deps, "acct", "docs/note.txt", "hello")

	// Create a watch.
	body := `{"path":"docs/note.txt"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/watches", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var create struct {
		Status string     `json:"status"`
		Result watchEntry `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &create))
	w := create.Result

	// Mutate the watched file so the next stat differs.
	time.Sleep(5 * time.Millisecond) // ensure modtime advances
	require.NoError(t, deps.RAGFS.Write(context.Background(), w.Path, bytes.NewReader([]byte("hello world")), 0o644))

	// Stream events over a real httptest.Server so the SSE goroutine owns
	// the response body. Reading from the live response.Body eliminates the
	// data race that httptest.ResponseRecorder triggers when one goroutine
	// writes the response while another reads Body.String() in a poll loop.
	srv := httptest.NewServer(r)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/v1/watches/"+w.ID+"/events", nil)
	require.NoError(t, err)
	req.Header.Set(identity.HeaderAccount, "acct")

	type readResult struct {
		body string
		err  error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		resp, err := srv.Client().Do(req)
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
				if strings.Contains(string(buf), "event: change") {
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
		t.Fatalf("timed out waiting for change event")
	case res := <-resultCh:
		require.NoError(t, res.err)
		assert.Contains(t, res.body, "event: change")
	}
}

func TestWatches_NilDepsReturns501(t *testing.T) {
	r := newWatchesTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/watches", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
