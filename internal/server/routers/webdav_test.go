package routers

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

// newWebDAVTestRouter builds a gin engine with the WebDAV router mounted at
// /webdav/resources/* on top of a fresh memfs. It mirrors newTestRouter but
// keeps WebDAV-specific setup isolated so resources_test.go stays untouched.
func newWebDAVTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest())
	mnt := ragfs.NewMountableFS()
	require.NoError(t, mnt.Mount("/", memfs.New("webdav-test")))
	RegisterWebDAV(r, &Deps{RAGFS: mnt})
	return r
}

// propfindResponse is the subset of the WebDAV multistatus XML we assert on.
type propfindResponse struct {
	XMLName xml.Name `xml:"multistatus"`
	Responses []struct {
		Href string `xml:"href"`
	} `xml:"response"`
}

// propfind issues a PROPFIND request and parses the multistatus body.
func propfind(t *testing.T, r *gin.Engine, target, depth string) (*propfindResponse, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest("PROPFIND", target, strings.NewReader(`<?xml version="1.0" encoding="utf-8"?><propfind xmlns="DAV:"><prop><displayname/></prop></propfind>`))
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", depth)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusMultiStatus, rec.Code, "PROPFIND body: %s", rec.Body.String())
	var resp propfindResponse
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return &resp, rec
}

func TestWebDAV_MkcolAndGet(t *testing.T) {
	r := newWebDAVTestRouter(t)

	// MKCOL a directory.
	req := httptest.NewRequest("MKCOL", "/webdav/resources/docs", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// GET the directory listing — webdav returns 200 for a PROPFIND; for
	// GET on a directory the memfs-backed file returns the empty body.
	// Instead we PROPFIND to confirm the resource exists.
	resp, _ := propfind(t, r, "/webdav/resources/docs", "0")
	require.Len(t, resp.Responses, 1)
	assert.Contains(t, resp.Responses[0].Href, "/webdav/resources/docs")
}

func TestWebDAV_PutAndGet(t *testing.T) {
	r := newWebDAVTestRouter(t)

	// PUT a file.
	body := "hello webdav"
	req := httptest.NewRequest("PUT", "/webdav/resources/hello.txt", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// GET it back.
	req = httptest.NewRequest("GET", "/webdav/resources/hello.txt", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, body, rec.Body.String())
}

func TestWebDAV_PropfindDepth1(t *testing.T) {
	r := newWebDAVTestRouter(t)

	// Setup: MKCOL a directory and PUT two files inside it.
	mkreq := httptest.NewRequest("MKCOL", "/webdav/resources/notes", nil)
	mkrec := httptest.NewRecorder()
	r.ServeHTTP(mkrec, mkreq)
	require.Equal(t, http.StatusCreated, mkrec.Code, mkrec.Body.String())

	for _, name := range []string{"a.txt", "b.txt"} {
		req := httptest.NewRequest("PUT", "/webdav/resources/notes/"+name, strings.NewReader("content "+name))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	// PROPFIND depth=1 should return the directory plus both children.
	resp, _ := propfind(t, r, "/webdav/resources/notes", "1")
	require.Len(t, resp.Responses, 3, "expected dir + 2 children, got %d", len(resp.Responses))
	hrefs := make([]string, 0, len(resp.Responses))
	for _, r := range resp.Responses {
		hrefs = append(hrefs, r.Href)
	}
	assert.Contains(t, hrefs, "/webdav/resources/notes/")
	assert.Contains(t, hrefs, "/webdav/resources/notes/a.txt")
	assert.Contains(t, hrefs, "/webdav/resources/notes/b.txt")
}

func TestWebDAV_Delete(t *testing.T) {
	r := newWebDAVTestRouter(t)

	// PUT a file.
	req := httptest.NewRequest("PUT", "/webdav/resources/trash.txt", strings.NewReader("garbage"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// DELETE it.
	req = httptest.NewRequest("DELETE", "/webdav/resources/trash.txt", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// GET should now 404.
	req = httptest.NewRequest("GET", "/webdav/resources/trash.txt", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWebDAV_MoveAndCopy(t *testing.T) {
	r := newWebDAVTestRouter(t)

	// PUT a source file.
	req := httptest.NewRequest("PUT", "/webdav/resources/src.txt", strings.NewReader("move me"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// COPY src.txt -> copy.txt (source must remain).
	req = httptest.NewRequest("COPY", "/webdav/resources/src.txt", nil)
	req.Header.Set("Destination", "http://example.com/webdav/resources/copy.txt")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// MOVE src.txt -> moved.txt (source should be gone).
	req = httptest.NewRequest("MOVE", "/webdav/resources/src.txt", nil)
	req.Header.Set("Destination", "http://example.com/webdav/resources/moved.txt")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Verify: copy.txt and moved.txt exist, src.txt does not.
	for _, p := range []string{"/webdav/resources/copy.txt", "/webdav/resources/moved.txt"} {
		req = httptest.NewRequest("GET", p, nil)
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equalf(t, http.StatusOK, rec.Code, "GET %s: %s", p, rec.Body.String())
		assert.Equal(t, "move me", rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/webdav/resources/src.txt", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWebDAV_GetMissingFile(t *testing.T) {
	r := newWebDAVTestRouter(t)

	req := httptest.NewRequest("GET", "/webdav/resources/does-not-exist.txt", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWebDAV_Options(t *testing.T) {
	r := newWebDAVTestRouter(t)

	// OPTIONS exposes the WebDAV DAV class header so clients can discover
	// which methods are supported.
	req := httptest.NewRequest("OPTIONS", "/webdav/resources/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	davHeader := rec.Header().Get("DAV")
	assert.NotEmpty(t, davHeader, "DAV header should be set")
	// Allow header should advertise WebDAV verbs.
	allow := rec.Header().Get("Allow")
	assert.Contains(t, allow, "PROPFIND")
}

// TestWebDAV_NilDepsReturns501 confirms that when RAGFS is not configured the
// route still responds (501) so the server boots.
func TestWebDAV_NilDepsReturns501(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest())
	RegisterWebDAV(r, &Deps{})

	req := httptest.NewRequest("GET", "/webdav/resources/anything", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// Suppress unused-import warnings for io in case future tests drop usage.
var _ = io.Discard
var _ = bytes.NewReader
