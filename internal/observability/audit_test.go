package observability

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

func TestAuditMiddlewareRecordsPost(t *testing.T) {
	var buf bytes.Buffer
	sink := NewAuditSinkFor(&buf)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("request_id", "rid-1"); c.Next() })
	r.Use(AuditMiddleware(sink))
	r.POST("/resource", func(c *gin.Context) { c.Status(http.StatusCreated) })

	req := httptest.NewRequest(http.MethodPost, "/resource", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set(identity.HeaderUser, "user1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry), "output: %s", buf.String())
	assert.Equal(t, "audit_event", entry["msg"])
	assert.Equal(t, "rid-1", entry["request_id"])
	assert.Equal(t, "acct", entry["account"])
	assert.Equal(t, "user1", entry["user"])
	assert.Equal(t, "POST", entry["action"])
	assert.Equal(t, "/resource", entry["resource"])
	assert.EqualValues(t, http.StatusCreated, entry["status"])
}

func TestAuditMiddlewareSkipsGet(t *testing.T) {
	var buf bytes.Buffer
	sink := NewAuditSinkFor(&buf)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("request_id", "rid-2"); c.Next() })
	r.Use(AuditMiddleware(sink))
	r.GET("/resource", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, buf.String())
}

func TestAuditMiddlewareSkipsHeadAndOptions(t *testing.T) {
	for _, method := range []string{http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			var buf bytes.Buffer
			sink := NewAuditSinkFor(&buf)

			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.Use(func(c *gin.Context) { c.Set("request_id", "rid-3"); c.Next() })
			r.Use(AuditMiddleware(sink))
			r.Handle(method, "/resource", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(method, "/resource", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			assert.Empty(t, buf.String())
		})
	}
}

func TestAuditMiddlewareRecordsAllMutating(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			var buf bytes.Buffer
			sink := NewAuditSinkFor(&buf)

			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.Use(func(c *gin.Context) { c.Set("request_id", "rid-4"); c.Next() })
			r.Use(AuditMiddleware(sink))
			r.Handle(method, "/resource", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(method, "/resource", nil)
			req.Header.Set(identity.HeaderAccount, "acct")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			require.NotEmpty(t, buf.String(), "method %s should be audited", method)
		})
	}
}

func TestAuditMiddlewareNilSink(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AuditMiddleware(nil))
	r.POST("/resource", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodPost, "/resource", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
