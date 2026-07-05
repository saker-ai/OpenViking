package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubVerifier implements APIKeyVerifier for tests.
type stubVerifier struct {
	keys map[string]string // raw key -> account id
}

func (s stubVerifier) VerifyAPIKey(ctx context.Context, key string) (string, error) {
	if acct, ok := s.keys[key]; ok {
		return acct, nil
	}
	return "", ErrInvalidToken
}

func newAuthEngine(cfg AuthConfig) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Auth(cfg))
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/readyz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/metrics", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/data", func(c *gin.Context) { c.String(http.StatusOK, "data:"+c.GetHeader("X-OpenViking-Account")) })
	return r
}

func TestAuthAcceptsValidAPIKey(t *testing.T) {
	verifier := stubVerifier{keys: map[string]string{"sk-valid": "acct-123"}}
	r := newAuthEngine(AuthConfig{APIKey: verifier})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Authorization", "Bearer sk-valid")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "data:acct-123", rec.Body.String())
}

func TestAuthRejectsInvalidAPIKey(t *testing.T) {
	verifier := stubVerifier{keys: map[string]string{"sk-valid": "acct-123"}}
	r := newAuthEngine(AuthConfig{APIKey: verifier})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Authorization", "Bearer sk-wrong")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNAUTHORIZED")
}

func TestAuthRejectsMissingToken(t *testing.T) {
	verifier := stubVerifier{keys: map[string]string{"sk-valid": "acct-123"}}
	r := newAuthEngine(AuthConfig{APIKey: verifier})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthSkipsHealthz(t *testing.T) {
	verifier := stubVerifier{keys: map[string]string{"sk-valid": "acct-123"}}
	r := newAuthEngine(AuthConfig{APIKey: verifier})

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "path %s should bypass auth", path)
	}
}

func TestAuthSkipsExplicitSkipPaths(t *testing.T) {
	verifier := stubVerifier{keys: map[string]string{"sk-valid": "acct-123"}}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Auth(AuthConfig{
		APIKey:    verifier,
		SkipPaths: []string{"/public"},
	}))
	r.GET("/public", func(c *gin.Context) { c.String(http.StatusOK, "public") })
	r.GET("/private", func(c *gin.Context) { c.String(http.StatusOK, "private") })

	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/public", nil))
	assert.Equal(t, http.StatusOK, rec1.Code)

	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/private", nil))
	assert.Equal(t, http.StatusUnauthorized, rec2.Code)
}

func TestAuthNilVerifiersPassthrough(t *testing.T) {
	r := newAuthEngine(AuthConfig{})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// No verifiers configured -> middleware passes through (P12 stub mode).
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthAcceptsXAPIKeyHeader(t *testing.T) {
	verifier := stubVerifier{keys: map[string]string{"sk-valid": "acct-123"}}
	r := newAuthEngine(AuthConfig{APIKey: verifier})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("X-Api-Key", "sk-valid")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "data:acct-123", rec.Body.String())
}

func TestVerifyAPIKeyHashSHA256(t *testing.T) {
	key := "sk-secret"
	sum := sha256.Sum256([]byte(key))
	stored := hex.EncodeToString(sum[:])

	ok, err := VerifyAPIKeyHash(key, stored, "sha256")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = VerifyAPIKeyHash("sk-wrong", stored, "sha256")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestVerifyAPIKeyHashBcrypt(t *testing.T) {
	key := "sk-secret"
	stored, err := HashAPIKey(key, "bcrypt")
	require.NoError(t, err)

	ok, err := VerifyAPIKeyHash(key, stored, "bcrypt")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = VerifyAPIKeyHash("sk-wrong", stored, "bcrypt")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestVerifyAPIKeyHashArgon2id(t *testing.T) {
	key := "sk-secret"
	stored, err := HashAPIKey(key, "argon2id")
	require.NoError(t, err)

	ok, err := VerifyAPIKeyHash(key, stored, "argon2id")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = VerifyAPIKeyHash("sk-wrong", stored, "argon2id")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestVerifyAPIKeyHashUnsupported(t *testing.T) {
	_, err := VerifyAPIKeyHash("k", "h", "md5")
	assert.Error(t, err)
}

func TestOAuthStubReturnsUnavailable(t *testing.T) {
	// Default OAuth verifier (noOAuth) should reject tokens, so a request
	// with a Bearer token and no API key verifier gets 401.
	r := newAuthEngine(AuthConfig{})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Authorization", "Bearer some-oauth-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// With nil APIKey + nil OAuth, middleware passes through. Test the
	// stub verifier directly instead.
	stub := noOAuth{}
	_, err := stub.VerifyToken(context.Background(), "some-token")
	assert.ErrorIs(t, err, errOAuthUnavailable)
}

func TestAuthFallsBackToOAuth(t *testing.T) {
	// API key verifier rejects, OAuth verifier succeeds.
	oauthStub := oauthVerifier{tokens: map[string]string{"tok-abc": "acct-from-oauth"}}
	r := newAuthEngine(AuthConfig{
		APIKey: stubVerifier{keys: map[string]string{"sk-valid": "acct-123"}},
		OAuth:  oauthStub,
	})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Authorization", "Bearer tok-abc")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "data:acct-from-oauth", rec.Body.String())
}

type oauthVerifier struct {
	tokens map[string]string
}

func (o oauthVerifier) VerifyToken(ctx context.Context, token string) (string, error) {
	if acct, ok := o.tokens[token]; ok {
		return acct, nil
	}
	return "", errors.New("oauth token invalid")
}
