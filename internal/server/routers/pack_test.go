package routers

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func newPackTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterPack(api, deps)
	RegisterResources(api, deps)
	return r
}

// buildTar builds an in-memory tar with the given file entries.
func buildTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Size:     int64(len(content)),
			Mode:     0o644,
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// readTarNames reads a tar stream and returns the list of regular file
// entry names.
func readTarNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	tr := tar.NewReader(r)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeReg {
			names = append(names, hdr.Name)
		}
	}
	return names
}

func TestPack_ExportStream(t *testing.T) {
	deps := newTestDeps(t)
	r := newPackTestRouter(t, deps)
	seedResource(t, r, "acct", "a.txt", "hello")
	seedResource(t, r, "acct", "b.txt", "world")

	body := `{"paths":["a.txt","b.txt"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pack/export", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/x-tar", rec.Header().Get("Content-Type"))
	names := readTarNames(t, rec.Body)
	assert.ElementsMatch(t, []string{"accounts/acct/resources/a.txt", "accounts/acct/resources/b.txt"}, names)
}

func TestPack_ExportSavedAndDownload(t *testing.T) {
	deps := newTestDeps(t)
	r := newPackTestRouter(t, deps)
	seedResource(t, r, "acct", "a.txt", "hello")

	// export with id -> saved
	body := `{"paths":["a.txt"],"id":"p1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pack/export", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// status
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pack/p1", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// download
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pack/p1/download", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/x-tar", rec.Header().Get("Content-Type"))
	names := readTarNames(t, rec.Body)
	assert.Contains(t, names, "accounts/acct/resources/a.txt")
}

func TestPack_Import(t *testing.T) {
	deps := newTestDeps(t)
	r := newPackTestRouter(t, deps)

	tarBytes := buildTar(t, map[string]string{"docs/x.txt": "hi", "docs/y.txt": "yo"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pack/import", bytes.NewReader(tarBytes))
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Imported []string `json:"imported"`
		Root     string   `json:"root"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "/accounts/acct/resources", body.Root)
	assert.ElementsMatch(t,
		[]string{"/accounts/acct/resources/docs/x.txt", "/accounts/acct/resources/docs/y.txt"},
		body.Imported)

	// Verify via resources stat.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/resources/docs/x.txt", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestPack_StatusNotFound(t *testing.T) {
	deps := newTestDeps(t)
	r := newPackTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pack/missing", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPack_ExportValidation(t *testing.T) {
	deps := newTestDeps(t)
	r := newPackTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pack/export",
		bytes.NewBufferString(`{"paths":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestPack_NilDepsReturnsError(t *testing.T) {
	r := newPackTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pack/whatever", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
