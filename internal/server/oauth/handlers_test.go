package oauth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
)

const (
	testBasePath    = "/api/v1/oauth"
	testExternalURL = "https://openviking.test"
)

func newTestServer(t *testing.T, clients ...config.ClientConfig) *Server {
	t.Helper()
	cfg := &config.OAuthConfig{
		Issuer:  testExternalURL,
		Clients: clients,
	}
	srv, err := New(cfg, testBasePath, testExternalURL)
	require.NoError(t, err)
	return srv
}

func newTestRouter(srv *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group(testBasePath)
	g.POST("/register", srv.RegisterHandler)
	g.GET("/authorize", srv.AuthorizeHandler)
	g.POST("/authorize", srv.AuthorizeHandler)
	g.GET("/callback", srv.CallbackHandler)
	g.POST("/token", srv.TokenHandler)
	g.POST("/revoke", srv.RevokeHandler)
	g.GET("/keys", srv.JWKSHandler)
	r.GET("/.well-known/oauth-authorization-server", srv.AuthorizationServerMetadataHandler)
	r.GET("/.well-known/oauth-protected-resource", srv.ProtectedResourceMetadataHandler)
	return r
}

func doRequest(t *testing.T, r http.Handler, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestDCR_Register_ConfidentialClient(t *testing.T) {
	srv := newTestServer(t)
	r := newTestRouter(srv)

	body := `{"client_name":"test-client","redirect_uris":["https://app.test/cb"],"grant_types":["client_credentials"],"scope":["fosite"]}`
	rec := doRequest(t, r, http.MethodPost, "/api/v1/oauth/register", body, map[string]string{
		"Content-Type": "application/json",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp ClientResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.ID, "client_id should be issued")
	assert.NotEmpty(t, resp.Secret, "client_secret should be issued")
	assert.Contains(t, resp.ID, "ov_")
	assert.Contains(t, resp.GrantTypes, "client_credentials")
}

func TestDCR_Register_PublicClient(t *testing.T) {
	srv := newTestServer(t)
	r := newTestRouter(srv)

	body := `{"client_name":"test-public","token_endpoint_auth_method":"none","redirect_uris":["https://app.test/cb"],"grant_types":["authorization_code"]}`
	rec := doRequest(t, r, http.MethodPost, "/api/v1/oauth/register", body, map[string]string{
		"Content-Type": "application/json",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp ClientResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Secret, "public client should not receive a secret")
	assert.True(t, resp.TokenEndpointAuthMethod == "none")
}

func TestDCR_Register_ClientIDConflict(t *testing.T) {
	srv := newTestServer(t, config.ClientConfig{
		ID:         "existing",
		Secret:     "secret",
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"fosite"},
	})
	r := newTestRouter(srv)

	body := `{"client_id":"existing","client_name":"dupe"}`
	rec := doRequest(t, r, http.MethodPost, "/api/v1/oauth/register", body, map[string]string{
		"Content-Type": "application/json",
	})
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "CLIENT_ID_CONFLICT")
}

func TestDCR_Register_InvalidJSON(t *testing.T) {
	srv := newTestServer(t)
	r := newTestRouter(srv)

	rec := doRequest(t, r, http.MethodPost, "/api/v1/oauth/register", `{not json`, map[string]string{
		"Content-Type": "application/json",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestToken_ClientCredentialsGrant(t *testing.T) {
	srv := newTestServer(t, config.ClientConfig{
		ID:         "cc-client",
		Secret:     "foobar",
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"fosite", "openviking"},
	})
	r := newTestRouter(srv)

	// Exchange client_credentials for an access token.
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {"fosite"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cc-client", "foobar")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var tok map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tok))
	accessToken, ok := tok["access_token"].(string)
	require.True(t, ok, "access_token should be a string")
	assert.NotEmpty(t, accessToken)
	assert.Equal(t, "bearer", tok["token_type"])
	assert.Equal(t, "fosite", tok["scope"])
}

func TestToken_RejectsInvalidClient(t *testing.T) {
	srv := newTestServer(t, config.ClientConfig{
		ID:         "cc-client",
		Secret:     "foobar",
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"fosite"},
	})
	r := newTestRouter(srv)

	form := url.Values{"grant_type": {"client_credentials"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cc-client", "wrong-secret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// fosite returns 401 when client authentication fails.
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

func TestRevoke_InvalidToken(t *testing.T) {
	srv := newTestServer(t, config.ClientConfig{
		ID:         "cc-client",
		Secret:     "foobar",
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"fosite"},
	})
	r := newTestRouter(srv)

	form := url.Values{"token": {"invalid-token"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/revoke", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cc-client", "foobar")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// RFC 7009: revocation of an invalid token returns 200 (no leak).
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestRevoke_AfterTokenExchange(t *testing.T) {
	srv := newTestServer(t, config.ClientConfig{
		ID:         "cc-client",
		Secret:     "foobar",
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"fosite"},
	})
	r := newTestRouter(srv)

	// 1) Get a token.
	form := url.Values{"grant_type": {"client_credentials"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cc-client", "foobar")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var tok map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tok))
	token := tok["access_token"].(string)

	// 2) Revoke it.
	form = url.Values{"token": {token}}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/oauth/revoke", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cc-client", "foobar")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestJWKS_ReturnsEmptySet(t *testing.T) {
	srv := newTestServer(t)
	r := newTestRouter(srv)

	rec := doRequest(t, r, http.MethodGet, "/api/v1/oauth/keys", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var jwks JWKS
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &jwks))
	assert.Empty(t, jwks.Keys, "HMAC strategy yields an empty JWKS")
}

func TestWellKnown_AuthorizationServerMetadata(t *testing.T) {
	srv := newTestServer(t)
	r := newTestRouter(srv)

	rec := doRequest(t, r, http.MethodGet, "/.well-known/oauth-authorization-server", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var meta AuthorizationServerMetadata
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.Equal(t, testExternalURL, meta.Issuer)
	assert.Equal(t, "https://openviking.test/api/v1/oauth/token", meta.TokenEndpoint)
	assert.Equal(t, "https://openviking.test/api/v1/oauth/authorize", meta.AuthorizationEndpoint)
	assert.Equal(t, "https://openviking.test/api/v1/oauth/register", meta.RegistrationEndpoint)
	assert.Contains(t, meta.CodeChallengeMethodsSupported, "S256")
	assert.Contains(t, meta.GrantTypesSupported, "client_credentials")
}

func TestWellKnown_ProtectedResourceMetadata(t *testing.T) {
	srv := newTestServer(t)
	r := newTestRouter(srv)

	rec := doRequest(t, r, http.MethodGet, "/.well-known/oauth-protected-resource", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var meta ProtectedResourceMetadata
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.Equal(t, testExternalURL, meta.Resource)
	assert.Contains(t, meta.AuthorizationServers, testExternalURL)
}

func TestAuthorize_RequiresProvider(t *testing.T) {
	srv := newTestServer(t)
	r := newTestRouter(srv)

	// Without a provider query param, the authorize endpoint returns
	// 400 so the OAuth package has zero Not-Implemented status
	// responses anywhere.
	rec := doRequest(t, r, http.MethodGet, "/api/v1/oauth/authorize?response_type=code&client_id=x", "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "provider")
}

func TestAuthorize_UnknownProvider(t *testing.T) {
	srv := newTestServer(t)
	r := newTestRouter(srv)

	rec := doRequest(t, r, http.MethodGet, "/api/v1/oauth/authorize?provider=unknown", "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unknown provider")
}

func TestNew_NilConfigReturnsError(t *testing.T) {
	_, err := New(nil, testBasePath, testExternalURL)
	assert.Error(t, err)
}

func TestNew_SeededClientLookup(t *testing.T) {
	srv := newTestServer(t, config.ClientConfig{
		ID:         "seeded",
		Secret:     "shh",
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"fosite"},
	})
	c, err := srv.store.GetClient(nil, "seeded")
	require.NoError(t, err)
	assert.Equal(t, "seeded", c.GetID())
	assert.False(t, c.IsPublic())
}

func TestNew_PopulatesIssuerDefault(t *testing.T) {
	cfg := &config.OAuthConfig{Issuer: ""}
	srv, err := New(cfg, testBasePath, testExternalURL)
	require.NoError(t, err)
	assert.Equal(t, testExternalURL, srv.Issuer())
}
