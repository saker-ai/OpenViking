package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

func TestFromHeaders(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderAccount, "acct")
	h.Set(HeaderUser, "user1")
	h.Set(HeaderActorPeer, "peerA")
	id := FromHeaders(h)
	assert.Equal(t, domain.Identifier{Account: "acct", User: "user1", ActorPeer: "peerA"}, id)
}

func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	id := domain.Identifier{Account: "acct", User: "u"}
	ctx = WithIdentity(ctx, id)
	got, ok := FromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, id, got)
}

func TestFromContextMissing(t *testing.T) {
	_, ok := FromContext(context.Background())
	assert.False(t, ok)
}

func TestMiddlewareSetsIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.GET("/probe", func(c *gin.Context) {
		id, ok := FromContext(c.Request.Context())
		require.True(t, ok)
		c.JSON(200, gin.H{"account": id.Account, "user": id.User})
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(HeaderAccount, "acct")
	req.Header.Set(HeaderUser, "u")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"account":"acct"`)
	assert.Contains(t, rec.Body.String(), `"user":"u"`)
}

func TestMiddlewareMissingAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.GET("/probe", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
