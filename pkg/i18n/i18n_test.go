package i18n

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"
)

func TestTranslateDefaultEnglish(t *testing.T) {
	require.NoError(t, Load(DefaultLocale))
	got := Translate("en", "hello")
	assert.Equal(t, "Hello", got)
}

func TestTranslateChineseFallback(t *testing.T) {
	require.NoError(t, Load("zh-CN"))
	got := Translate("zh-CN", "hello")
	assert.Equal(t, "你好", got)
}

func TestTranslateMissingKeyReturnsID(t *testing.T) {
	require.NoError(t, Load(DefaultLocale))
	got := Translate("en", "no.such.key")
	assert.Equal(t, "no.such.key", got)
}

func TestTranslateWithData(t *testing.T) {
	require.NoError(t, Load("en"))
	got := Translate("en", "ov.add_resource.added", map[string]interface{}{"path": "/tmp/x"})
	assert.Equal(t, "Resource added: /tmp/x", got)
}

func TestSupportedIncludesEnglish(t *testing.T) {
	require.NoError(t, Load(DefaultLocale))
	tags := Supported()
	assert.Contains(t, tags, language.English)
}

func TestParseLocaleUnd(t *testing.T) {
	assert.Equal(t, language.Und, parseLocale(""))
	assert.Equal(t, language.Und, parseLocale("garbage"))
	assert.Equal(t, language.English, parseLocale("en"))
	assert.Equal(t, language.AmericanEnglish, parseLocale("en-US"))
	// zh-CN parses as the regional tag, not the script form zh-Hans.
	assert.Equal(t, language.MustParse("zh-CN"), parseLocale("zh-CN"))
	assert.Equal(t, language.Chinese, parseLocale("zh"))
	// Encodings stripped: zh_CN.UTF-8 -> zh-CN.
	assert.Equal(t, language.MustParse("zh-CN"), parseLocale("zh_CN.UTF-8"))
}

func TestDetectLocaleFromEnv(t *testing.T) {
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Setenv(env, "")
	}
	assert.Equal(t, language.English, detectLocaleFromEnv())
	t.Setenv("LANG", "zh_CN.UTF-8")
	// zh_CN.UTF-8 strips encoding + underscore -> zh-CN.
	assert.Equal(t, language.MustParse("zh-CN"), detectLocaleFromEnv())
}
