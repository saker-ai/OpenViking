package cli

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelsLoginCmd_Feishu(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.OAuthURLBuilder = stubOAuthBuilder{url: "https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=APP123"}

	cmd := silence(ChannelsCmd(rt))
	require.NoError(t, Execute(cmd, []string{"login", "feishu"}))
	body := out.String()
	assert.Contains(t, body, "https://open.feishu.cn/open-apis/authen/v1/authorize")
	assert.Contains(t, body, "Tokens will be stored at:")
}

func TestChannelsLoginCmd_UnsupportedChannel(t *testing.T) {
	rt, _, _ := newTestRuntimeNoServer(t)
	rt.OAuthURLBuilder = stubOAuthBuilder{}

	cmd := silence(ChannelsCmd(rt))
	err := Execute(cmd, []string{"login", "wechat"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported channel")
}

func TestChannelsLoginCmd_StubForNonFeishu(t *testing.T) {
	rt, _, _ := newTestRuntimeNoServer(t)
	// defaultOAuthURLBuilder is used when OAuthURLBuilder is nil. For
	// slack/dingtalk/telegram it returns a TODO error.
	cmd := silence(ChannelsCmd(rt))
	err := Execute(cmd, []string{"login", "slack"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet implemented")
}

func TestChannelsLoginCmd_RedirectOverride(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.OAuthURLBuilder = stubOAuthBuilder{url: "https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=APP123"}

	cmd := silence(ChannelsCmd(rt))
	require.NoError(t, Execute(cmd, []string{"login", "feishu", "--redirect-uri", "https://example.com/cb"}))
	body := out.String()
	assert.Contains(t, body, "redirect_uri=https")
	// The URL is the last non-empty line of output.
	parsed, err := url.Parse(lastLine(body))
	require.NoError(t, err)
	vals, err := url.ParseQuery(parsed.RawQuery)
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/cb", vals.Get("redirect_uri"))
}

func TestChannelsLoginCmd_State(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.OAuthURLBuilder = stubOAuthBuilder{url: "https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=APP123"}

	cmd := silence(ChannelsCmd(rt))
	require.NoError(t, Execute(cmd, []string{"login", "feishu", "--state", "csrf-abc"}))
	assert.Contains(t, out.String(), "state=csrf-abc")
}

func TestChannelTokensDir_Env(t *testing.T) {
	t.Setenv("OV_CHANNEL_TOKENS_DIR", "/tmp/ov-channels-test")
	got, err := ChannelTokensDir()
	require.NoError(t, err)
	assert.Equal(t, "/tmp/ov-channels-test", got)
}

func TestChannelTokensDir_Default(t *testing.T) {
	t.Setenv("OV_CHANNEL_TOKENS_DIR", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	got, err := ChannelTokensDir()
	require.NoError(t, err)
	want := filepath.Join(dir, ".openviking", "channels")
	assert.Equal(t, want, got)
}

func TestSaveChannelToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OV_CHANNEL_TOKENS_DIR", dir)
	token := map[string]any{
		"refresh_token": "rt-xyz",
		"access_token":  "at-abc",
		"expires_at":    "2026-07-12T00:00:00Z",
	}
	require.NoError(t, SaveChannelToken("feishu", token))

	data, err := os.ReadFile(filepath.Join(dir, "feishu.json"))
	require.NoError(t, err)
	body := string(data)
	assert.Contains(t, body, "rt-xyz")
	assert.Contains(t, body, "at-abc")
}

func TestChannelsCmd_Help(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	cmd := ChannelsCmd(rt)
	cmd.SetOut(out)
	cmd.SetErr(out)
	require.NoError(t, Execute(cmd, []string{"--help"}))
	assert.Contains(t, out.String(), "login")
}

// lastLine returns the last non-empty line of s, trimmed of whitespace.
// Used to extract the URL from a multi-line CLI output.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// stubOAuthBuilder is a test OAuthURLBuilder that returns a fixed URL.
type stubOAuthBuilder struct {
	url string
}

func (b stubOAuthBuilder) Build(_ context.Context, _, _ string) (string, error) {
	if b.url == "" {
		return "", nil
	}
	return b.url, nil
}
