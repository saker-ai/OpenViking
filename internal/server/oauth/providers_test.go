package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMockProviderServer starts an httptest.Server that responds to the
// provider's token + userinfo endpoints. The returned configureFn
// rewrites the provider's URLs to point at the server.
func newMockProviderServer(t *testing.T, tokenHandler, userInfoHandler http.HandlerFunc) (*httptest.Server, func(baseURL string)) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", tokenHandler)
	mux.HandleFunc("/userinfo", userInfoHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func(baseURL string) {}
}

func TestFeishuProvider_Exchange(t *testing.T) {
	var (
		gotCode          string
		gotClientID      string
		gotClientSecret  string
		gotGrantType     string
	)
	tokenSrv, _ := newMockProviderServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			gotCode = r.FormValue("code")
			gotClientID = r.FormValue("client_id")
			gotClientSecret = r.FormValue("client_secret")
			gotGrantType = r.FormValue("grant_type")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"access_token":      "feishu-at-1",
					"refresh_token":     "feishu-rt-1",
					"token_type":        "Bearer",
					"expires_in":        7200,
					"refresh_expires_in": 2592000,
				},
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			assert.Equal(t, "Bearer feishu-at-1", auth)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"open_id": "ou_test_123",
					"name":    "Alice",
					"email":   "alice@example.com",
				},
			})
		},
	)
	p := newFeishuProvider(ProviderConfig{
		Name:         ProviderFeishu,
		ClientID:     "feishu-cid",
		ClientSecret: "feishu-cs",
		AuthURL:      tokenSrv.URL + "/auth",
		TokenURL:     tokenSrv.URL + "/token",
		UserInfoURL:  tokenSrv.URL + "/userinfo",
	}, tokenSrv.Client())

	tok, err := p.Exchange(context.Background(), "code-xyz", "", "https://app.test/cb")
	require.NoError(t, err)
	require.NotNil(t, tok)
	assert.Equal(t, "feishu-at-1", tok.AccessToken)
	assert.Equal(t, "feishu-rt-1", tok.RefreshToken)
	assert.Equal(t, "Bearer", tok.TokenType)
	assert.Equal(t, 7200, tok.ExpiresIn)
	assert.Equal(t, "ou_test_123", tok.UserID)
	assert.Equal(t, "Alice", tok.Subject)

	assert.Equal(t, "code-xyz", gotCode)
	assert.Equal(t, "feishu-cid", gotClientID)
	assert.Equal(t, "feishu-cs", gotClientSecret)
	assert.Equal(t, "authorization_code", gotGrantType)
}

func TestFeishuProvider_Refresh_OK(t *testing.T) {
	tokenSrv, _ := newMockProviderServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			assert.Equal(t, "refresh_token", r.FormValue("grant_type"))
			assert.Equal(t, "feishu-rt-old", r.FormValue("refresh_token"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"access_token":  "feishu-at-new",
					"refresh_token": "feishu-rt-new",
					"token_type":    "Bearer",
					"expires_in":    7200,
				},
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"open_id": "ou_test_123", "name": "Alice"},
			})
		},
	)
	p := newFeishuProvider(ProviderConfig{
		Name:         ProviderFeishu,
		ClientID:     "feishu-cid",
		ClientSecret: "feishu-cs",
		TokenURL:     tokenSrv.URL + "/token",
		UserInfoURL:  tokenSrv.URL + "/userinfo",
	}, tokenSrv.Client())

	tok, err := p.Refresh(context.Background(), "feishu-rt-old")
	require.NoError(t, err)
	assert.Equal(t, "feishu-at-new", tok.AccessToken)
	assert.Equal(t, "feishu-rt-new", tok.RefreshToken)
}

func TestFeishuProvider_Refresh_PermanentError(t *testing.T) {
	tokenSrv, _ := newMockProviderServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 40013,
				"msg":  "invalid refresh_token",
			})
		},
		func(w http.ResponseWriter, r *http.Request) {},
	)
	p := newFeishuProvider(ProviderConfig{
		Name:         ProviderFeishu,
		ClientID:     "feishu-cid",
		ClientSecret: "feishu-cs",
		TokenURL:     tokenSrv.URL + "/token",
		UserInfoURL:  tokenSrv.URL + "/userinfo",
	}, tokenSrv.Client())

	_, err := p.Refresh(context.Background(), "feishu-rt-old")
	require.Error(t, err)
	assert.True(t, IsPermanentRefreshError(err), "expected permanent error, got %v", err)
}

func TestGoogleProvider_Exchange(t *testing.T) {
	tokenSrv, _ := newMockProviderServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = r.ParseForm()
			assert.Equal(t, "authorization_code", r.FormValue("grant_type"))
			assert.Equal(t, "code-xyz", r.FormValue("code"))
			// golang.org/x/oauth2 v0.36 sends client_id + client_secret
			// in the Authorization header (HTTP Basic) by default for
			// confidential clients. Verify they're present in either
			// the body or the header.
			if r.FormValue("client_id") != "google-cid" || r.FormValue("client_secret") != "google-cs" {
				auth := r.Header.Get("Authorization")
				assert.NotEmpty(t, auth, "client credentials must be sent in body or Authorization header")
				// Basic auth header value is base64(client_id:client_secret).
				assert.Contains(t, auth, "Basic ", "expected Basic auth header")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "google-at-1",
				"refresh_token": "google-rt-1",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"scope":         "openid email profile",
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			auth := r.Header.Get("Authorization")
			assert.Equal(t, "Bearer google-at-1", auth)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sub":   "google-sub-1",
				"email": "bob@example.com",
				"name":  "Bob",
			})
		},
	)
	p := newGoogleProvider(ProviderConfig{
		Name:         ProviderGoogle,
		ClientID:     "google-cid",
		ClientSecret: "google-cs",
		Scopes:       []string{"openid", "email", "profile"},
		AuthURL:      tokenSrv.URL + "/auth",
		TokenURL:     tokenSrv.URL + "/token",
		UserInfoURL:  tokenSrv.URL + "/userinfo",
	}, tokenSrv.Client())

	tok, err := p.Exchange(context.Background(), "code-xyz", "", "https://app.test/cb")
	require.NoError(t, err)
	assert.Equal(t, "google-at-1", tok.AccessToken)
	assert.Equal(t, "google-rt-1", tok.RefreshToken)
	assert.Equal(t, "google-sub-1", tok.UserID)
	assert.Equal(t, "Bob", tok.Subject)
}

func TestGoogleProvider_Refresh_PermanentError(t *testing.T) {
	tokenSrv, _ := newMockProviderServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "invalid_grant",
			})
		},
		func(w http.ResponseWriter, r *http.Request) {},
	)
	p := newGoogleProvider(ProviderConfig{
		Name:         ProviderGoogle,
		ClientID:     "google-cid",
		ClientSecret: "google-cs",
		TokenURL:     tokenSrv.URL + "/token",
		UserInfoURL:  tokenSrv.URL + "/userinfo",
	}, tokenSrv.Client())

	_, err := p.Refresh(context.Background(), "google-rt-old")
	require.Error(t, err)
	assert.True(t, IsPermanentRefreshError(err), "expected permanent error, got %v", err)
}

func TestSlackProvider_Exchange(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		assert.Equal(t, "slack-cid", r.FormValue("client_id"))
		assert.Equal(t, "code-xyz", r.FormValue("code"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":            true,
			"access_token":  "slack-at-1",
			"refresh_token": "slack-rt-1",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"authed_user": map[string]any{
				"id":            "U123",
				"scope":         "openid,email",
				"access_token":  "slack-user-at",
				"refresh_token": "slack-user-rt",
				"token_type":    "Bearer",
				"expires_in":    3600,
			},
		})
	}))
	t.Cleanup(tokenSrv.Close)
	p := newSlackProvider(ProviderConfig{
		Name:        ProviderSlack,
		ClientID:    "slack-cid",
		TokenURL:    tokenSrv.URL + "/oauth.v2.access",
		UserInfoURL: tokenSrv.URL + "/userinfo",
	}, tokenSrv.Client())

	tok, err := p.Exchange(context.Background(), "code-xyz", "", "https://app.test/cb")
	require.NoError(t, err)
	assert.Equal(t, "slack-at-1", tok.AccessToken)
	assert.Equal(t, "slack-rt-1", tok.RefreshToken)
	assert.Equal(t, "U123", tok.UserID)
}

func TestSlackProvider_Refresh_OK(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		assert.Equal(t, "refresh_token", r.FormValue("grant_type"))
		assert.Equal(t, "slack-rt-old", r.FormValue("refresh_token"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":            true,
			"access_token":  "slack-at-new",
			"refresh_token": "slack-rt-new",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"authed_user": map[string]any{
				"id": "U123",
			},
		})
	}))
	t.Cleanup(tokenSrv.Close)
	p := newSlackProvider(ProviderConfig{
		Name:     ProviderSlack,
		ClientID: "slack-cid",
		TokenURL: tokenSrv.URL + "/oauth.v2.access",
	}, tokenSrv.Client())

	tok, err := p.Refresh(context.Background(), "slack-rt-old")
	require.NoError(t, err)
	assert.Equal(t, "slack-at-new", tok.AccessToken)
	assert.Equal(t, "slack-rt-new", tok.RefreshToken)
}

func TestSlackProvider_Refresh_PermanentError(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "invalid_refresh_token",
		})
	}))
	t.Cleanup(tokenSrv.Close)
	p := newSlackProvider(ProviderConfig{
		Name:     ProviderSlack,
		ClientID: "slack-cid",
		TokenURL: tokenSrv.URL + "/oauth.v2.access",
	}, tokenSrv.Client())

	_, err := p.Refresh(context.Background(), "slack-rt-old")
	require.Error(t, err)
	assert.True(t, IsPermanentRefreshError(err), "expected permanent error, got %v", err)
}

func TestDingTalkProvider_Exchange(t *testing.T) {
	tokenSrv, _ := newMockProviderServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			body := make(map[string]any)
			_ = json.NewDecoder(r.Body).Decode(&body)
			assert.Equal(t, "dingtalk-cid", body["clientId"])
			assert.Equal(t, "dingtalk-cs", body["clientSecret"])
			assert.Equal(t, "authorization_code", body["grantType"])
			assert.Equal(t, "code-xyz", body["code"])
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accessToken":  "dingtalk-at-1",
				"refreshToken": "dingtalk-rt-1",
				"expiresIn":    7200,
				"tokenType":    "Bearer",
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "dingtalk-at-1", r.Header.Get("x-acs-dingtalk-access-token"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"openId":  "dingtalk-open-1",
				"unionId": "dingtalk-union-1",
				"nick":    "Carol",
				"email":   "carol@example.com",
			})
		},
	)
	p := newDingTalkProvider(ProviderConfig{
		Name:         ProviderDingTalk,
		ClientID:     "dingtalk-cid",
		ClientSecret: "dingtalk-cs",
		AuthURL:      tokenSrv.URL + "/auth",
		TokenURL:     tokenSrv.URL + "/token",
		UserInfoURL:  tokenSrv.URL + "/userinfo",
	}, tokenSrv.Client())

	tok, err := p.Exchange(context.Background(), "code-xyz", "", "https://app.test/cb")
	require.NoError(t, err)
	assert.Equal(t, "dingtalk-at-1", tok.AccessToken)
	assert.Equal(t, "dingtalk-rt-1", tok.RefreshToken)
	assert.Equal(t, "dingtalk-open-1", tok.UserID)
	assert.Equal(t, "Carol", tok.Subject)
}

func TestDingTalkProvider_Refresh_OK(t *testing.T) {
	tokenSrv, _ := newMockProviderServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			body := make(map[string]any)
			_ = json.NewDecoder(r.Body).Decode(&body)
			assert.Equal(t, "refresh_token", body["grantType"])
			assert.Equal(t, "dingtalk-rt-old", body["refreshToken"])
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accessToken":  "dingtalk-at-new",
				"refreshToken": "dingtalk-rt-new",
				"expiresIn":    7200,
				"tokenType":    "Bearer",
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"openId": "dingtalk-open-1",
				"nick":   "Carol",
			})
		},
	)
	p := newDingTalkProvider(ProviderConfig{
		Name:         ProviderDingTalk,
		ClientID:     "dingtalk-cid",
		ClientSecret: "dingtalk-cs",
		TokenURL:     tokenSrv.URL + "/token",
		UserInfoURL:  tokenSrv.URL + "/userinfo",
	}, tokenSrv.Client())

	tok, err := p.Refresh(context.Background(), "dingtalk-rt-old")
	require.NoError(t, err)
	assert.Equal(t, "dingtalk-at-new", tok.AccessToken)
	assert.Equal(t, "dingtalk-rt-new", tok.RefreshToken)
}

func TestBuildProviders_SkipsUnknown(t *testing.T) {
	cfgs := []ProviderConfig{
		{Name: ProviderFeishu, ClientID: "x"},
		{Name: "unknown", ClientID: "y"},
	}
	providers := BuildProviders(cfgs)
	assert.Contains(t, providers, ProviderFeishu)
	_, ok := providers["unknown"]
	assert.False(t, ok)
}

func TestProvider_AuthURL_IncludesState(t *testing.T) {
	p := newFeishuProvider(ProviderConfig{
		Name:     ProviderFeishu,
		ClientID: "cid",
		AuthURL:  "https://example.test/auth",
	}, http.DefaultClient)
	u := p.AuthURL("state-xyz", "challenge-abc", "https://app.test/cb")
	assert.Contains(t, u, "https://example.test/auth?")
	assert.Contains(t, u, "state=state-xyz")
	assert.Contains(t, u, "code_challenge=challenge-abc")
	assert.Contains(t, u, "code_challenge_method=S256")
	// Feishu uses app_id; google/slack/dingtalk use client_id. Either
	// is acceptable as long as the value is present.
	assert.True(t, containsParam(u, "app_id", "cid") || containsParam(u, "client_id", "cid"),
		"auth URL must include app_id or client_id=cid: %s", u)
}

// containsParam reports whether the query string of rawURL contains a
// parameter named name with value value. Used to assert on the feishu
// auth URL without coupling to the provider's specific field name.
func containsParam(rawURL, name, value string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return parsed.Query().Get(name) == value
}

// TestProviderNames_Const ensures the metric label values match the
// provider names used in the store. A drift here would silently break
// oauthTokenRefreshTotal.
func TestProviderNames_Const(t *testing.T) {
	assert.Equal(t, "feishu", ProviderFeishu)
	assert.Equal(t, "google", ProviderGoogle)
	assert.Equal(t, "slack", ProviderSlack)
	assert.Equal(t, "dingtalk", ProviderDingTalk)
}

// TestRefreshError_ImplementsError ensures *RefreshError satisfies the
// error interface so it can be wrapped/returned by Provider.Refresh.
func TestRefreshError_ImplementsError(t *testing.T) {
	var err error = &RefreshError{Provider: "feishu", Message: "boom"}
	assert.Contains(t, err.Error(), "feishu")
	assert.Contains(t, err.Error(), "boom")
	assert.True(t, IsPermanentRefreshError(err))
}
