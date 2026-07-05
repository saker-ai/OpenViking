package accessors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFeishuAuthState(t *testing.T) {
	t.Parallel()
	s := NewFeishuAuthState("access-1", "refresh-1")
	assert.Equal(t, FeishuAuthProvider, s.Provider)
	assert.Equal(t, "access-1", s.AccessToken)
	assert.Equal(t, "refresh-1", s.RefreshToken)
	assert.True(t, s.ExpiresAt.IsZero(), "initial state has unknown expiry")
}

func TestIsFeishuAuthState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   map[string]any
		want bool
	}{
		{"nil", nil, false},
		{"empty", map[string]any{}, false},
		{"other provider", map[string]any{"provider": "google"}, false},
		{"feishu provider", map[string]any{"provider": "feishu"}, true},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, IsFeishuAuthState(c.in), c.name)
	}
}

func TestFeishuAuthStateNeedsRefresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		state map[string]any
		want  bool
	}{
		{
			name:  "nil state",
			state: nil,
			want:  true,
		},
		{
			name:  "no expires_at",
			state: map[string]any{"provider": "feishu"},
			want:  true,
		},
		{
			name: "empty expires_at",
			state: map[string]any{
				"provider":   "feishu",
				"expires_at": "",
			},
			want: true,
		},
		{
			name: "expired",
			state: map[string]any{
				"provider":   "feishu",
				"expires_at": now.Add(-time.Hour).Format(time.RFC3339Nano),
			},
			want: true,
		},
		{
			name: "within skew",
			state: map[string]any{
				"provider":   "feishu",
				"expires_at": now.Add(2 * time.Minute).Format(time.RFC3339Nano),
			},
			want: true,
		},
		{
			name: "fresh",
			state: map[string]any{
				"provider":   "feishu",
				"expires_at": now.Add(time.Hour).Format(time.RFC3339Nano),
			},
			want: false,
		},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, FeishuAuthStateNeedsRefresh(c.state, now), c.name)
	}
}

func TestApplyFeishuRefreshedToken(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	original := map[string]any{
		"provider":      "feishu",
		"access_token":  "old-access",
		"refresh_token": "old-refresh",
		"expires_at":    now.Add(-time.Hour).Format(time.RFC3339Nano),
		"watch_id":      "w-123",
	}
	refreshed := FeishuRefreshedToken{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresIn:    7200,
	}
	out := ApplyFeishuRefreshedToken(original, refreshed, now)

	// Original map is not mutated.
	assert.Equal(t, "old-access", original["access_token"], "original not mutated")

	// New map has refreshed values.
	assert.Equal(t, "feishu", out["provider"])
	assert.Equal(t, "new-access", out["access_token"])
	assert.Equal(t, "new-refresh", out["refresh_token"])

	// ExpiresAt is now + 7200s.
	parsed, err := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
	require.NoError(t, err)
	assert.True(t, parsed.Equal(now.Add(7200*time.Second)))

	// Extra fields are preserved.
	assert.Equal(t, "w-123", out["watch_id"])
}

func TestParseExpiresAt(t *testing.T) {
	t.Parallel()
	t.Run("nil", func(t *testing.T) {
		_, err := parseExpiresAt(nil)
		assert.Error(t, err)
	})
	t.Run("empty string", func(t *testing.T) {
		_, err := parseExpiresAt("")
		assert.Error(t, err)
	})
	t.Run("RFC3339 with Z", func(t *testing.T) {
		ts, err := parseExpiresAt("2026-07-05T12:00:00Z")
		require.NoError(t, err)
		assert.Equal(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC), ts)
	})
	t.Run("RFC3339 nano", func(t *testing.T) {
		ts, err := parseExpiresAt("2026-07-05T12:00:00.123456789Z")
		require.NoError(t, err)
		assert.Equal(t, "2026-07-05 12:00:00.123456789 +0000 UTC", ts.String())
	})
	t.Run("already time.Time", func(t *testing.T) {
		in := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
		ts, err := parseExpiresAt(in)
		require.NoError(t, err)
		assert.Equal(t, in, ts)
	})
	t.Run("garbage", func(t *testing.T) {
		_, err := parseExpiresAt("not-a-date")
		assert.Error(t, err)
	})
	t.Run("unsupported type", func(t *testing.T) {
		_, err := parseExpiresAt(42)
		assert.Error(t, err)
	})
}

func TestIsPermanentRefreshError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		msg  string
		want bool
	}{
		{0, "", false},
		{400, "invalid refresh_token", true},
		{400, "refresh token expired", true},
		{400, "unauthorized", true},
		{400, "token revoked", true},
		{500, "service unavailable", false},
		{429, "rate limited", false},
		{400, "refresh_token mismatch", true},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, isPermanentRefreshError(c.code, c.msg),
			"code=%d msg=%q", c.code, c.msg)
	}
}

func TestFeishuTokenRefreshErrorFormatting(t *testing.T) {
	t.Parallel()
	wrapped := errors.New("network timeout")
	e := &FeishuTokenRefreshError{
		Message:    "refresh request failed",
		Permanent:  false,
		Underlying: wrapped,
	}
	assert.ErrorIs(t, e, wrapped, "Unwrap should expose underlying")
	assert.Contains(t, e.Error(), "refresh request failed")
	assert.Contains(t, e.Error(), "network timeout")
}

func TestNewFeishuOAuthClientFromEnvMissing(t *testing.T) {
	t.Setenv("OV_PARSE_FEISHU_APP_ID", "")
	t.Setenv("OV_PARSE_FEISHU_APP_SECRET", "")
	_, err := NewFeishuOAuthClientFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing app_id/app_secret")
}

func TestNewFeishuOAuthClientFromEnvOK(t *testing.T) {
	t.Setenv("OV_PARSE_FEISHU_APP_ID", "app-123")
	t.Setenv("OV_PARSE_FEISHU_APP_SECRET", "secret-456")
	t.Setenv("OV_PARSE_FEISHU_DOMAIN", "https://open.larksuite.com")
	c, err := NewFeishuOAuthClientFromEnv()
	require.NoError(t, err)
	assert.Equal(t, "app-123", c.credentials.AppID)
	assert.Equal(t, "secret-456", c.credentials.AppSecret)
	assert.Equal(t, "https://open.larksuite.com", c.credentials.Domain)
}

func TestRefreshUserAccessTokenEmptyToken(t *testing.T) {
	t.Parallel()
	c := NewFeishuOAuthClient(FeishuAppCredentials{
		AppID: "app", AppSecret: "secret", Domain: "https://open.feishu.cn",
	})
	_, err := c.RefreshUserAccessToken(context.Background(), "  ")
	require.Error(t, err)
	var feishuErr *FeishuTokenRefreshError
	require.True(t, errors.As(err, &feishuErr), "should be *FeishuTokenRefreshError")
	assert.True(t, feishuErr.Permanent, "empty refresh token is permanent")
}

// TestRefreshUserAccessTokenHTTPMock spins up an httptest server that
// mimics the lark authen.v1.refresh_access_token endpoint and asserts
// that the client correctly parses the response.
func TestRefreshUserAccessTokenHTTPMock(t *testing.T) {
	t.Parallel()
	var (
		gotBody string
		gotPath string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"code": 0,
			"msg": "ok",
			"data": {
				"access_token": "new-access-token",
				"refresh_token": "new-refresh-token",
				"expires_in": 7200,
				"token_type": "Bearer"
			}
		}`))
	}))
	defer server.Close()

	c := NewFeishuOAuthClient(FeishuAppCredentials{
		AppID: "app", AppSecret: "secret", Domain: server.URL,
	})
	// Force the lark client to point at our test server by constructing
	// it directly with the test URL.
	c.client = newLarkClient("app", "secret", server.URL, server.Client())

	out, err := c.RefreshUserAccessToken(context.Background(), "old-refresh-token")
	require.NoError(t, err)
	assert.Equal(t, "new-access-token", out.AccessToken)
	assert.Equal(t, "new-refresh-token", out.RefreshToken)
	assert.Equal(t, 7200, out.ExpiresIn)

	// The refresh endpoint is /open-apis/authen/v1/refresh_access_token.
	assert.Contains(t, gotPath, "refresh_access_token")
	// The body should be JSON with grant_type=refresh_token and our refresh_token.
	assert.Contains(t, gotBody, "refresh_token")
	assert.Contains(t, gotBody, "old-refresh-token")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(gotBody), &parsed))
	assert.Equal(t, "refresh_token", parsed["grant_type"], "grant_type is the fixed value")
}

// TestRefreshUserAccessTokenServerError verifies that a non-zero code in
// the response is surfaced as a *FeishuTokenRefreshError.
func TestRefreshUserAccessTokenServerError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"code": 400,
			"msg": "invalid refresh_token"
		}`))
	}))
	defer server.Close()

	c := NewFeishuOAuthClient(FeishuAppCredentials{
		AppID: "app", AppSecret: "secret", Domain: server.URL,
	})
	c.client = newLarkClient("app", "secret", server.URL, server.Client())

	_, err := c.RefreshUserAccessToken(context.Background(), "bad-token")
	require.Error(t, err)
	var feishuErr *FeishuTokenRefreshError
	require.True(t, errors.As(err, &feishuErr))
	assert.True(t, feishuErr.Permanent, "invalid refresh_token is permanent")
	assert.True(t, strings.Contains(feishuErr.Message, "400") || strings.Contains(feishuErr.Message, "invalid"),
		"error message should mention code or msg, got %q", feishuErr.Message)
}
