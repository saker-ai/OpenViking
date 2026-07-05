// Package accessors — Feishu user-token refresh for watch tasks.
//
// Watch tasks that fetch Feishu resources on behalf of a user (rather than
// the app tenant) hold a user_access_token + refresh_token pair. The
// access_token expires; this file implements the refresh flow described
// in openviking/resource/feishu_watch_auth.py.
//
// The refresh call goes through the same lark client used by the Feishu
// accessor (tenant tokens) — the SDK routes the request through
// authen.v1.refresh_access_token.create, which accepts a refresh_token
// and returns a fresh access_token/refresh_token pair.
package accessors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkauthen "github.com/larksuite/oapi-sdk-go/v3/service/authen/v1"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// FeishuAuthProvider is the value stored in auth_state["provider"] to
// distinguish Feishu user-token watches from other providers.
const FeishuAuthProvider = "feishu"

// FeishuRefreshSkew is how long before expiry we proactively refresh.
const FeishuRefreshSkew = 5 * time.Minute

// FeishuRefreshGrantType is the fixed grant_type value for the refresh
// request.
const FeishuRefreshGrantType = "refresh_token"

// FeishuAppCredentials holds the app credentials used to refresh user
// tokens. Loaded from config or environment.
type FeishuAppCredentials struct {
	AppID           string
	AppSecret       string
	Domain          string
	RequestTimeout  time.Duration
}

// FeishuRefreshedToken is the result of a successful refresh call.
type FeishuRefreshedToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
}

// FeishuTokenRefreshError wraps refresh failures. Permanent=true means the
// refresh_token is invalid/expired/revoked — the watch task should stop
// rather than retry.
type FeishuTokenRefreshError struct {
	Message    string
	Permanent  bool
	Underlying error
}

func (e *FeishuTokenRefreshError) Error() string {
	if e.Underlying != nil {
		return fmt.Sprintf("feishu token refresh: %s: %v", e.Message, e.Underlying)
	}
	return "feishu token refresh: " + e.Message
}

func (e *FeishuTokenRefreshError) Unwrap() error { return e.Underlying }

// FeishuAuthState is the watch auth state for Feishu user-token watches.
// Stored in the watch entry's auth_state field.
type FeishuAuthState struct {
	Provider     string    `json:"provider"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"` // zero = unknown
}

// NewFeishuAuthState creates an initial auth state from a freshly obtained
// token pair (e.g. right after the user completes OAuth). ExpiresAt is
// zero — the first watch tick will refresh immediately.
func NewFeishuAuthState(accessToken, refreshToken string) *FeishuAuthState {
	return &FeishuAuthState{
		Provider:     FeishuAuthProvider,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}
}

// IsFeishuAuthState reports whether the generic map[string]any auth_state
// belongs to a Feishu watch. Used by the watch scheduler to dispatch to
// the right refresh handler.
func IsFeishuAuthState(state map[string]any) bool {
	if state == nil {
		return false
	}
	p, _ := state["provider"].(string)
	return p == FeishuAuthProvider
}

// FeishuAuthStateNeedsRefresh reports whether the access_token is expired
// or about to expire (within FeishuRefreshSkew). A zero ExpiresAt means
// unknown — refresh proactively.
func FeishuAuthStateNeedsRefresh(state map[string]any, now time.Time) bool {
	if state == nil {
		return true
	}
	raw, ok := state["expires_at"]
	if !ok {
		return true
	}
	expiresAt, err := parseExpiresAt(raw)
	if err != nil || expiresAt.IsZero() {
		return true
	}
	return !expiresAt.After(now.Add(FeishuRefreshSkew))
}

// ApplyFeishuRefreshedToken returns a new auth_state map with the refreshed
// tokens and a freshly computed ExpiresAt. The original state is not
// mutated.
func ApplyFeishuRefreshedToken(state map[string]any, refreshed FeishuRefreshedToken, now time.Time) map[string]any {
	out := make(map[string]any, len(state)+4)
	for k, v := range state {
		out[k] = v
	}
	expiresAt := now.Add(time.Duration(maxInt(0, refreshed.ExpiresIn)) * time.Second)
	out["provider"] = FeishuAuthProvider
	out["access_token"] = refreshed.AccessToken
	out["refresh_token"] = refreshed.RefreshToken
	out["expires_at"] = expiresAt.UTC().Format(time.RFC3339Nano)
	return out
}

// FeishuOAuthClient wraps a *lark.Client to refresh user access tokens.
// The lark client is shared with the FeishuAccessor when possible; the
// SDK routes the refresh call through authen.v1.refresh_access_token.
type FeishuOAuthClient struct {
	credentials FeishuAppCredentials
	client      *lark.Client
}

// NewFeishuOAuthClient constructs a refresh client with explicit
// credentials. domain may be empty (defaults to https://open.feishu.cn).
func NewFeishuOAuthClient(creds FeishuAppCredentials) *FeishuOAuthClient {
	if creds.Domain == "" {
		creds.Domain = "https://open.feishu.cn"
	}
	if creds.RequestTimeout <= 0 {
		creds.RequestTimeout = 30 * time.Second
	}
	return &FeishuOAuthClient{credentials: creds}
}

// NewFeishuOAuthClientFromEnv reads credentials from OV_PARSE_FEISHU_*
// environment variables. Returns an error suitable for surfacing to the
// watch scheduler when credentials are missing.
func NewFeishuOAuthClientFromEnv() (*FeishuOAuthClient, error) {
	appID := strings.TrimSpace(os.Getenv("OV_PARSE_FEISHU_APP_ID"))
	appSecret := strings.TrimSpace(os.Getenv("OV_PARSE_FEISHU_APP_SECRET"))
	baseDomain := strings.TrimSpace(os.Getenv("OV_PARSE_FEISHU_DOMAIN"))
	if appID == "" || appSecret == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"feishu watch auth: missing app_id/app_secret (set OV_PARSE_FEISHU_APP_ID / OV_PARSE_FEISHU_APP_SECRET)")
	}
	return NewFeishuOAuthClient(FeishuAppCredentials{
		AppID:          appID,
		AppSecret:      appSecret,
		Domain:         baseDomain,
		RequestTimeout: 30 * time.Second,
	}), nil
}

// RefreshUserAccessToken refreshes the user access token using the
// refresh_token grant. Returns the new token pair or a
// *FeishuTokenRefreshError (permanent=true if the refresh_token itself is
// invalid).
func (c *FeishuOAuthClient) RefreshUserAccessToken(ctx context.Context, refreshToken string) (FeishuRefreshedToken, error) {
	rt := strings.TrimSpace(refreshToken)
	if rt == "" {
		return FeishuRefreshedToken{}, &FeishuTokenRefreshError{
			Message:   "refresh token is missing",
			Permanent: true,
		}
	}
	client := c.client
	if client == nil {
		client = newLarkClient(c.credentials.AppID, c.credentials.AppSecret, c.credentials.Domain, nil)
	}
	if client == nil || client.Authen == nil || client.Authen.RefreshAccessToken == nil {
		return FeishuRefreshedToken{}, &FeishuTokenRefreshError{
			Message:   "lark client missing authen service",
			Permanent: true,
		}
	}
	body := larkauthen.NewCreateRefreshAccessTokenReqBodyBuilder().
		GrantType(FeishuRefreshGrantType).
		RefreshToken(rt).
		Build()
	req := larkauthen.NewCreateRefreshAccessTokenReqBuilder().Body(body).Build()
	resp, err := client.Authen.RefreshAccessToken.Create(ctx, req)
	if err != nil {
		// The lark SDK returns a *larkcore.CodeError when the API responds
		// with a non-zero code (e.g. invalid refresh_token). Extract the
		// code/msg so we can flag permanent failures.
		var ce larkcore.CodeError
		if errors.As(err, &ce) {
			return FeishuRefreshedToken{}, &FeishuTokenRefreshError{
				Message:   fmt.Sprintf("refresh failed: code=%d msg=%s", ce.Code, ce.Msg),
				Permanent: isPermanentRefreshError(ce.Code, ce.Msg),
			}
		}
		return FeishuRefreshedToken{}, &FeishuTokenRefreshError{
			Message:    "refresh request failed",
			Permanent:  false,
			Underlying: err,
		}
	}
	if !resp.Success() {
		return FeishuRefreshedToken{}, &FeishuTokenRefreshError{
			Message:   fmt.Sprintf("refresh failed: code=%d msg=%s", resp.Code, resp.Msg),
			Permanent: isPermanentRefreshError(resp.Code, resp.Msg),
		}
	}
	if resp.Data == nil {
		return FeishuRefreshedToken{}, &FeishuTokenRefreshError{
			Message:   "refresh response missing data",
			Permanent: false,
		}
	}
	access := strings.TrimSpace(ptrString(resp.Data.AccessToken))
	newRefresh := strings.TrimSpace(ptrString(resp.Data.RefreshToken))
	if newRefresh == "" {
		// Spec: when the response omits refresh_token, the old one stays valid
		// until it expires. Reuse the input.
		newRefresh = rt
	}
	expiresIn := 0
	if resp.Data.ExpiresIn != nil {
		expiresIn = *resp.Data.ExpiresIn
	}
	if access == "" || expiresIn <= 0 {
		return FeishuRefreshedToken{}, &FeishuTokenRefreshError{
			Message:   "refresh response missing access_token or expires_in",
			Permanent: false,
		}
	}
	return FeishuRefreshedToken{
		AccessToken:  access,
		RefreshToken: newRefresh,
		ExpiresIn:    expiresIn,
	}, nil
}

// parseExpiresAt accepts the value stored in auth_state["expires_at"]:
// either an ISO-8601 string (preferred) or a time.Time. Returns the zero
// time and an error when the value cannot be interpreted.
func parseExpiresAt(v any) (time.Time, error) {
	switch x := v.(type) {
	case nil:
		return time.Time{}, errors.New("nil expires_at")
	case time.Time:
		return x, nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return time.Time{}, errors.New("empty expires_at")
		}
		// RFC3339 with optional 'Z' suffix; the Python side writes
		// datetime.isoformat() which is close but may not include
		// timezone for naive datetimes.
		s = strings.Replace(s, "Z", "+00:00", 1)
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t, err = time.Parse(time.RFC3339, s)
			if err != nil {
				return time.Time{}, err
			}
		}
		if t.Location() == time.UTC {
			return t, nil
		}
		return t.UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported expires_at type %T", v)
	}
}

// isPermanentRefreshError inspects the lark API code/msg to decide whether
// the refresh_token itself is invalid (so retries are pointless).
func isPermanentRefreshError(code int, msg string) bool {
	text := strings.ToLower(fmt.Sprintf("%d %s", code, msg))
	permanentTerms := []string{
		"invalid",
		"expired",
		"revoked",
		"unauthorized",
		"not exist",
		"not found",
		"mismatch",
		"refresh token",
		"refresh_token",
	}
	for _, term := range permanentTerms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

// ptrString dereferences a *string safely, returning "" for nil.
func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// maxInt returns the larger of two ints.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
