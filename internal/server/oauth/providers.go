// Package oauth — external OAuth provider adapters.
//
// This file defines the Provider interface and four concrete adapters
// (feishu, google, slack, dingtalk) used by the OAuth client flow
// (/api/v1/oauth/authorize, /callback, /token?grant_type=refresh_token).
//
// Each adapter knows how to:
//   - Build the provider's authorize URL (with state + PKCE challenge).
//   - Exchange an authorization code for an access_token + refresh_token.
//   - Refresh an expired access_token using the stored refresh_token.
//   - Map the provider's userinfo to a stable user_id + subject.
//
// All HTTP calls go through a configurable *http.Client so tests can point
// at an httptest.NewServer. No SDK is wired in for DingTalk (no mature Go
// SDK); feishu uses net/http directly against the open.feishu.cn API
// rather than the lark SDK because the SDK's refresh flow requires the
// app's internal client and would couple the OAuth client surface to the
// parse-accessor surface. google and slack use golang.org/x/oauth2 +
// slack-go/slack respectively for token exchange; userinfo is fetched via
// plain HTTP to avoid pulling the google people/v1 SDK just for the email
// field.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"golang.org/x/oauth2"
)

// Provider names. These label the oauthTokenRefreshTotal metric and
// disambiguate stored token rows.
const (
	ProviderFeishu    = "feishu"
	ProviderGoogle    = "google"
	ProviderSlack     = "slack"
	ProviderDingTalk  = "dingtalk"
)

// ErrProviderUnknown is returned when a provider name does not match any
// registered adapter.
var ErrProviderUnknown = errors.New("oauth: unknown provider")

// Provider abstracts an external OAuth2 provider. Implementations must be
// safe for concurrent use.
type Provider interface {
	// Name returns the provider identifier (e.g. "feishu").
	Name() string

	// AuthURL builds the provider's authorize endpoint URL with the given
	// state and PKCE code_challenge. The caller generates state + verifier.
	AuthURL(state, codeChallenge, redirectURI string) string

	// Exchange swaps an authorization code for a token pair. The returned
	// Token's RefreshToken may be empty when the provider does not issue
	// refresh tokens (e.g. DingTalk's older flow); the store keeps whatever
	// the provider returns.
	Exchange(ctx context.Context, code, codeVerifier, redirectURI string) (*Token, error)

	// Refresh exchanges a stored refresh_token for a new token pair. The
	// returned error must wrap a *RefreshError when the failure is permanent
	// (invalid / expired / revoked refresh_token) so the caller can stop
	// retrying and surface the right metric label.
	Refresh(ctx context.Context, refreshToken string) (*Token, error)

	// Scopes returns the provider's default scope string sent on the
	// authorize URL. Empty when the provider uses no scopes.
	Scopes() string
}

// Token is the provider-agnostic result of Exchange/Refresh. AccessToken
// and RefreshToken are plaintext; the store encrypts them at rest.
type Token struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int
	Scope        string
	UserID       string
	Subject      string
}

// RefreshError marks a refresh failure as permanent. The caller increments
// oauthTokenRefreshTotal with status="permanent_error" when this is set;
// otherwise status="error".
type RefreshError struct {
	Provider string
	Message  string
}

func (e *RefreshError) Error() string {
	return fmt.Sprintf("oauth refresh %s: %s", e.Provider, e.Message)
}

// IsPermanentRefreshError reports whether err wraps a *RefreshError.
func IsPermanentRefreshError(err error) bool {
	var re *RefreshError
	return errors.As(err, &re)
}

// ProviderConfig holds the credentials + endpoints for one provider.
// RedirectURIs is the list of allowed callback URLs; the caller picks one
// per authorize request.
type ProviderConfig struct {
	Name         string
	ClientID     string
	ClientSecret string
	RedirectURIs []string
	Scopes       []string
	// AuthURL / TokenURL override the provider's default endpoints. When
	// empty the provider's production endpoints are used. Tests override
	// these to point at an httptest server.
	AuthURL  string
	TokenURL string
	// UserInfoURL is the provider's userinfo endpoint. Optional — used by
	// providers that don't return user_id in the token response
	// (DingTalk, Feishu).
	UserInfoURL string
	// HTTPClient is the client used for outbound calls. nil uses http.DefaultClient.
	HTTPClient *http.Client
}

// BuildProviders constructs the Provider map from cfg. Unknown provider
// names are skipped (the caller logs them); the returned map is empty
// when no providers are configured.
func BuildProviders(cfgs []ProviderConfig) map[string]Provider {
	out := make(map[string]Provider, len(cfgs))
	for _, c := range cfgs {
		p, err := buildProvider(c)
		if err != nil {
			continue
		}
		out[p.Name()] = p
	}
	return out
}

func buildProvider(c ProviderConfig) (Provider, error) {
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	switch c.Name {
	case ProviderFeishu:
		return newFeishuProvider(c, hc), nil
	case ProviderGoogle:
		return newGoogleProvider(c, hc), nil
	case ProviderSlack:
		return newSlackProvider(c, hc), nil
	case ProviderDingTalk:
		return newDingTalkProvider(c, hc), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrProviderUnknown, c.Name)
	}
}

// ---------------------------------------------------------------------------
// Feishu
// ---------------------------------------------------------------------------

// feishuProvider implements Provider for feishu's open.feishu.cn OAuth.
// Feishu returns refresh_token on both authorize-code exchange and refresh;
// user_id is the open_id field from the token response's user_info.
type feishuProvider struct {
	cfg        ProviderConfig
	httpClient *http.Client
}

func newFeishuProvider(c ProviderConfig, hc *http.Client) *feishuProvider {
	c.Name = ProviderFeishu
	if c.AuthURL == "" {
		c.AuthURL = "https://open.feishu.cn/openapiservices/authen/v1/index"
	}
	if c.TokenURL == "" {
		c.TokenURL = "https://open.feishu.cn/openapiservices/authen/v1/oidc/access_token"
	}
	if c.UserInfoURL == "" {
		c.UserInfoURL = "https://open.feishu.cn/openapiservices/authen/v1/user_info"
	}
	return &feishuProvider{cfg: c, httpClient: hc}
}

func (p *feishuProvider) Name() string { return ProviderFeishu }
func (p *feishuProvider) Scopes() string { return "" }

func (p *feishuProvider) AuthURL(state, codeChallenge, redirectURI string) string {
	q := url.Values{
		"app_id":         {p.cfg.ClientID},
		"redirect_uri":   {redirectURI},
		"state":          {state},
		"response_type":  {"code"},
	}
	if codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}
	return p.cfg.AuthURL + "?" + q.Encode()
}

// feishuTokenResponse mirrors the body returned by /oidc/access_token.
type feishuTokenResponse struct {
	Code int `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int    `json:"expires_in"`
		RefreshExpiresIn int    `json:"refresh_expires_in"`
	} `json:"data"`
}

func (p *feishuProvider) Exchange(ctx context.Context, code, codeVerifier, redirectURI string) (*Token, error) {
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {p.cfg.ClientID},
		"client_secret": {p.cfg.ClientSecret},
	}
	if codeVerifier != "" {
		body.Set("code_verifier", codeVerifier)
	}
	tok, err := postForm(ctx, p.httpClient, p.cfg.TokenURL, body, &feishuTokenResponse{})
	if err != nil {
		return nil, err
	}
	fr := tok.(*feishuTokenResponse)
	if fr.Code != 0 {
		return nil, fmt.Errorf("feishu exchange: code=%d msg=%s", fr.Code, fr.Msg)
	}
	userID, subject, err := p.fetchUserInfo(ctx, fr.Data.AccessToken)
	if err != nil {
		return nil, err
	}
	return &Token{
		AccessToken:  fr.Data.AccessToken,
		RefreshToken: fr.Data.RefreshToken,
		TokenType:    defaultStr(fr.Data.TokenType, "Bearer"),
		ExpiresIn:    fr.Data.ExpiresIn,
		UserID:       userID,
		Subject:      subject,
	}, nil
}

func (p *feishuProvider) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	body := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {p.cfg.ClientID},
		"client_secret": {p.cfg.ClientSecret},
	}
	tok, err := postForm(ctx, p.httpClient, p.cfg.TokenURL, body, &feishuTokenResponse{})
	if err != nil {
		return nil, fmt.Errorf("feishu refresh: %w", err)
	}
	fr := tok.(*feishuTokenResponse)
	if fr.Code != 0 {
		return nil, &RefreshError{Provider: ProviderFeishu, Message: fmt.Sprintf("code=%d msg=%s", fr.Code, fr.Msg)}
	}
	userID, subject, err := p.fetchUserInfo(ctx, fr.Data.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("feishu refresh userinfo: %w", err)
	}
	return &Token{
		AccessToken:  fr.Data.AccessToken,
		RefreshToken: defaultStr(fr.Data.RefreshToken, refreshToken),
		TokenType:    defaultStr(fr.Data.TokenType, "Bearer"),
		ExpiresIn:    fr.Data.ExpiresIn,
		UserID:       userID,
		Subject:      subject,
	}, nil
}

type feishuUserInfo struct {
	Code int `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		OpenID string `json:"open_id"`
		Name   string `json:"name"`
		Email  string `json:"email"`
	} `json:"data"`
}

func (p *feishuProvider) fetchUserInfo(ctx context.Context, accessToken string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.UserInfoURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("feishu userinfo: status=%d body=%s", resp.StatusCode, string(body))
	}
	var ui feishuUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&ui); err != nil {
		return "", "", err
	}
	if ui.Code != 0 {
		return "", "", fmt.Errorf("feishu userinfo: code=%d msg=%s", ui.Code, ui.Msg)
	}
	return defaultStr(ui.Data.OpenID, ui.Data.Email), defaultStr(ui.Data.Name, ui.Data.Email), nil
}

// ---------------------------------------------------------------------------
// Google
// ---------------------------------------------------------------------------

// googleProvider uses golang.org/x/oauth2 for the standard authorize-code
// exchange + refresh. Userinfo (sub + email) is fetched from the
// googleapis userinfo endpoint.
type googleProvider struct {
	cfg        ProviderConfig
	httpClient *http.Client
}

func newGoogleProvider(c ProviderConfig, hc *http.Client) *googleProvider {
	c.Name = ProviderGoogle
	if c.AuthURL == "" {
		c.AuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
	}
	if c.TokenURL == "" {
		c.TokenURL = "https://oauth2.googleapis.com/token"
	}
	if c.UserInfoURL == "" {
		c.UserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	}
	return &googleProvider{cfg: c, httpClient: hc}
}

func (p *googleProvider) Name() string { return ProviderGoogle }
func (p *googleProvider) Scopes() string {
	if len(p.cfg.Scopes) > 0 {
		return strings.Join(p.cfg.Scopes, " ")
	}
	return "openid email profile"
}

func (p *googleProvider) AuthURL(state, codeChallenge, redirectURI string) string {
	q := url.Values{
		"client_id":     {p.cfg.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"state":         {state},
		"scope":         {p.Scopes()},
	}
	if codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}
	return p.cfg.AuthURL + "?" + q.Encode()
}

func (p *googleProvider) config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  p.cfg.AuthURL,
			TokenURL: p.cfg.TokenURL,
		},
		RedirectURL: firstRedirectURI(p.cfg.RedirectURIs),
		Scopes:      p.cfg.Scopes,
	}
}

func (p *googleProvider) Exchange(ctx context.Context, code, codeVerifier, redirectURI string) (*Token, error) {
	cfg := p.config()
	if redirectURI != "" {
		cfg.RedirectURL = redirectURI
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
	opts := []oauth2.AuthCodeOption{}
	if codeVerifier != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_verifier", codeVerifier))
	}
	tok, err := cfg.Exchange(ctx, code, opts...)
	if err != nil {
		return nil, fmt.Errorf("google exchange: %w", err)
	}
	userID, subject, err := p.fetchUserInfo(ctx, tok.AccessToken)
	if err != nil {
		return nil, err
	}
	return &Token{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    defaultStr(tok.TokenType, "Bearer"),
		ExpiresIn:    int(tok.Expiry.Sub(time.Now()).Seconds()),
		UserID:       userID,
		Subject:      subject,
	}, nil
}

func (p *googleProvider) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	cfg := p.config()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
	src := cfg.TokenSource(ctx, &oauth2.Token{
		RefreshToken: refreshToken,
	})
	tok, err := src.Token()
	if err != nil {
		if isOAuth2PermError(err) {
			return nil, &RefreshError{Provider: ProviderGoogle, Message: err.Error()}
		}
		return nil, fmt.Errorf("google refresh: %w", err)
	}
	userID, subject, err := p.fetchUserInfo(ctx, tok.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("google refresh userinfo: %w", err)
	}
	rt := tok.RefreshToken
	if rt == "" {
		rt = refreshToken
	}
	return &Token{
		AccessToken:  tok.AccessToken,
		RefreshToken: rt,
		TokenType:    defaultStr(tok.TokenType, "Bearer"),
		ExpiresIn:    int(tok.Expiry.Sub(time.Now()).Seconds()),
		UserID:       userID,
		Subject:      subject,
	}, nil
}

type googleUserInfo struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (p *googleProvider) fetchUserInfo(ctx context.Context, accessToken string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.UserInfoURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("google userinfo: status=%d body=%s", resp.StatusCode, string(body))
	}
	var ui googleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&ui); err != nil {
		return "", "", err
	}
	return defaultStr(ui.Sub, ui.Email), defaultStr(ui.Name, ui.Email), nil
}

// isOAuth2PermError reports whether an oauth2.RetrieveError indicates an
// invalid_grant (refresh_token expired/revoked). The oauth2 package wraps
// the underlying error with a *oauth2.RetrieveError whose Response body
// contains the standard error string.
func isOAuth2PermError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "invalid_grant") ||
		strings.Contains(text, "invalid refresh") ||
		strings.Contains(text, "expired") ||
		strings.Contains(text, "revoked")
}

// ---------------------------------------------------------------------------
// Slack
// ---------------------------------------------------------------------------

// slackProvider uses slack-go/slack for token exchange + userinfo. Slack
// does not issue refresh tokens for the standard OAuth flow (tokens are
// long-lived); the refresh surface uses RefreshOAuthV2TokenContext when
// the response carries a refresh_token (Slack rotating tokens), and
// otherwise returns the same token untouched.
type slackProvider struct {
	cfg        ProviderConfig
	httpClient *http.Client
}

func newSlackProvider(c ProviderConfig, hc *http.Client) *slackProvider {
	c.Name = ProviderSlack
	if c.AuthURL == "" {
		c.AuthURL = "https://slack.com/oauth/v2/authorize"
	}
	if c.TokenURL == "" {
		c.TokenURL = "https://slack.com/api/oauth.v2.access"
	}
	if c.UserInfoURL == "" {
		c.UserInfoURL = "https://slack.com/api/users.identity"
	}
	return &slackProvider{cfg: c, httpClient: hc}
}

func (p *slackProvider) Name() string { return ProviderSlack }
func (p *slackProvider) Scopes() string {
	if len(p.cfg.Scopes) > 0 {
		return strings.Join(p.cfg.Scopes, ",")
	}
	return "openid,email,profile"
}

func (p *slackProvider) AuthURL(state, codeChallenge, redirectURI string) string {
	q := url.Values{
		"client_id":     {p.cfg.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"state":         {state},
		"scope":         {p.Scopes()},
	}
	if codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}
	return p.cfg.AuthURL + "?" + q.Encode()
}

func (p *slackProvider) Exchange(ctx context.Context, code, codeVerifier, redirectURI string) (*Token, error) {
	// slack-go/slack expects the API URL without the trailing
	// /oauth.v2.access — it appends that itself. Strip the trailing
	// path so tests that point at an httptest server work.
	apiURL := p.cfg.TokenURL
	if strings.HasSuffix(apiURL, "/oauth.v2.access") {
		apiURL = strings.TrimSuffix(apiURL, "/oauth.v2.access") + "/"
	}
	opts := []slack.OAuthOption{slack.OAuthOptionAPIURL(apiURL)}
	if codeVerifier != "" {
		opts = append(opts, slack.OAuthOptionCodeVerifier(codeVerifier))
	}
	resp, err := slack.GetOAuthV2ResponseContext(ctx, p.httpClient, p.cfg.ClientID, p.cfg.ClientSecret, code, redirectURI, opts...)
	if err != nil {
		return nil, fmt.Errorf("slack exchange: %w", err)
	}
	if !resp.Ok {
		return nil, fmt.Errorf("slack exchange: %s", resp.Error)
	}
	userID, subject := slackUserIDs(resp)
	return &Token{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		TokenType:    defaultStr(resp.TokenType, "Bearer"),
		ExpiresIn:    respExpiresIn(resp.ExpiresIn),
		UserID:       userID,
		Subject:      subject,
	}, nil
}

func (p *slackProvider) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	if refreshToken == "" {
		return nil, &RefreshError{Provider: ProviderSlack, Message: "empty refresh token"}
	}
	apiURL := p.cfg.TokenURL
	if strings.HasSuffix(apiURL, "/oauth.v2.access") {
		apiURL = strings.TrimSuffix(apiURL, "/oauth.v2.access") + "/"
	}
	resp, err := slack.RefreshOAuthV2TokenContext(ctx, p.httpClient, p.cfg.ClientID, p.cfg.ClientSecret, refreshToken, slack.OAuthOptionAPIURL(apiURL))
	if err != nil {
		if isSlackPermError(err) {
			return nil, &RefreshError{Provider: ProviderSlack, Message: err.Error()}
		}
		return nil, fmt.Errorf("slack refresh: %w", err)
	}
	if !resp.Ok {
		if isSlackPermString(resp.Error) {
			return nil, &RefreshError{Provider: ProviderSlack, Message: resp.Error}
		}
		return nil, fmt.Errorf("slack refresh: %s", resp.Error)
	}
	userID, subject := slackUserIDs(resp)
	rt := defaultStr(resp.RefreshToken, refreshToken)
	return &Token{
		AccessToken:  resp.AccessToken,
		RefreshToken: rt,
		TokenType:    defaultStr(resp.TokenType, "Bearer"),
		ExpiresIn:    respExpiresIn(resp.ExpiresIn),
		UserID:       userID,
		Subject:      subject,
	}, nil
}

// slackUserIDs extracts the user_id + subject from a Slack OAuthV2
// response. The AuthedUser field is always populated (non-pointer
// struct), so we check its ID field for emptiness rather than nil.
func slackUserIDs(resp *slack.OAuthV2Response) (string, string) {
	if resp == nil {
		return "", ""
	}
	if resp.AuthedUser.ID != "" {
		subject := resp.AuthedUser.ID
		return resp.AuthedUser.ID, subject
	}
	return "", ""
}

// isSlackPermError reports whether a slack error indicates the
// refresh_token is invalid/expired/revoked. Slack returns "invalid_
// refresh_token" / "expired_refresh_token" in those cases.
func isSlackPermError(err error) bool {
	return isSlackPermString(err.Error())
}

func isSlackPermString(s string) bool {
	text := strings.ToLower(s)
	return strings.Contains(text, "invalid") ||
		strings.Contains(text, "expired") ||
		strings.Contains(text, "revoked") ||
		strings.Contains(text, "refresh_token")
}

func respExpiresIn(v int) int {
	if v > 0 {
		return v
	}
	return 0
}

// ---------------------------------------------------------------------------
// DingTalk
// ---------------------------------------------------------------------------

// dingTalkProvider implements Provider for DingTalk's OAuth flow. DingTalk
// issues refresh_token on exchange; refresh is supported via the
// /oauth2/refreshToken endpoint.
type dingTalkProvider struct {
	cfg        ProviderConfig
	httpClient *http.Client
}

func newDingTalkProvider(c ProviderConfig, hc *http.Client) *dingTalkProvider {
	c.Name = ProviderDingTalk
	if c.AuthURL == "" {
		c.AuthURL = "https://login.dingtalk.com/oauth2/auth"
	}
	if c.TokenURL == "" {
		c.TokenURL = "https://api.dingtalk.com/v1.0/oauth2/userAccessToken"
	}
	if c.UserInfoURL == "" {
		c.UserInfoURL = "https://api.dingtalk.com/v1.0/contact/users/me"
	}
	return &dingTalkProvider{cfg: c, httpClient: hc}
}

func (p *dingTalkProvider) Name() string { return ProviderDingTalk }
func (p *dingTalkProvider) Scopes() string { return "" }

func (p *dingTalkProvider) AuthURL(state, codeChallenge, redirectURI string) string {
	q := url.Values{
		"client_id":     {p.cfg.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"state":         {state},
		"scope":         {"openid"},
		"prompt":        {"consent"},
	}
	if codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}
	return p.cfg.AuthURL + "?" + q.Encode()
}

// dingTalkTokenResponse mirrors /v1.0/oauth2/userAccessToken.
type dingTalkTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int    `json:"expireIn"`
	TokenType    string `json:"tokenType"`
}

func (p *dingTalkProvider) Exchange(ctx context.Context, code, codeVerifier, redirectURI string) (*Token, error) {
	payload := map[string]string{
		"clientId":     p.cfg.ClientID,
		"clientSecret": p.cfg.ClientSecret,
		"code":         code,
		"grantType":    "authorization_code",
	}
	if codeVerifier != "" {
		payload["codeVerifier"] = codeVerifier
	}
	if redirectURI != "" {
		payload["redirectUri"] = redirectURI
	}
	tok, err := postJSON(ctx, p.httpClient, p.cfg.TokenURL, payload, &dingTalkTokenResponse{})
	if err != nil {
		return nil, fmt.Errorf("dingtalk exchange: %w", err)
	}
	dt := tok.(*dingTalkTokenResponse)
	userID, subject, err := p.fetchUserInfo(ctx, dt.AccessToken)
	if err != nil {
		return nil, err
	}
	return &Token{
		AccessToken:  dt.AccessToken,
		RefreshToken: dt.RefreshToken,
		TokenType:    defaultStr(dt.TokenType, "Bearer"),
		ExpiresIn:    dt.ExpiresIn,
		UserID:       userID,
		Subject:      subject,
	}, nil
}

func (p *dingTalkProvider) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	payload := map[string]string{
		"clientId":     p.cfg.ClientID,
		"clientSecret": p.cfg.ClientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}
	tok, err := postJSON(ctx, p.httpClient, p.cfg.TokenURL, payload, &dingTalkTokenResponse{})
	if err != nil {
		return nil, fmt.Errorf("dingtalk refresh: %w", err)
	}
	dt := tok.(*dingTalkTokenResponse)
	if dt.AccessToken == "" {
		return nil, &RefreshError{Provider: ProviderDingTalk, Message: "empty accessToken"}
	}
	userID, subject, err := p.fetchUserInfo(ctx, dt.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("dingtalk refresh userinfo: %w", err)
	}
	return &Token{
		AccessToken:  dt.AccessToken,
		RefreshToken: defaultStr(dt.RefreshToken, refreshToken),
		TokenType:    defaultStr(dt.TokenType, "Bearer"),
		ExpiresIn:    dt.ExpiresIn,
		UserID:       userID,
		Subject:      subject,
	}, nil
}

type dingTalkUserInfo struct {
	Nick      string `json:"nick"`
	UnionID   string `json:"unionId"`
	OpenID    string `json:"openId"`
	Email     string `json:"email"`
}

func (p *dingTalkProvider) fetchUserInfo(ctx context.Context, accessToken string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.UserInfoURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("x-acs-dingtalk-access-token", accessToken)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("dingtalk userinfo: status=%d body=%s", resp.StatusCode, string(body))
	}
	var ui dingTalkUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&ui); err != nil {
		return "", "", err
	}
	return defaultStr(defaultStr(ui.OpenID, ui.UnionID), ui.Email), defaultStr(ui.Nick, ui.Email), nil
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

// postForm posts form values to url and decodes JSON into out. out must be
// a pointer to a struct.
func postForm(ctx context.Context, hc *http.Client, target string, form url.Values, out any) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

// postJSON posts a JSON body and decodes the JSON response into out.
func postJSON(ctx context.Context, hc *http.Client, target string, payload any, out any) (any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(b))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

func defaultStr(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}

func firstRedirectURI(uris []string) string {
	if len(uris) == 0 {
		return ""
	}
	return uris[0]
}
