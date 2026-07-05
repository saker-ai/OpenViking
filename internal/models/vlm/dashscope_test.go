package vlm

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/saker-ai/ctxhub/internal/config"
)

func vlmDashscopeConfig(model, key string) config.VLMConfig {
	return config.VLMConfig{Model: model, APIKey: key}
}

func TestDashscopeClientDefaultsBaseURL(t *testing.T) {
	t.Parallel()
	c := NewDashscope(vlmDashscopeConfig("qwen-max", "sk-dash"), nil)
	assert.Equal(t, DefaultDashscopeBaseURL, c.BaseURL)
	assert.Equal(t, "qwen-max", c.Model)
	assert.Equal(t, "sk-dash", c.APIKey)
}

func TestDashscopeClientHonorsAPIBase(t *testing.T) {
	t.Parallel()
	cfg := vlmDashscopeConfig("qwen-max", "k")
	cfg.APIBase = "https://custom.example/v1/"
	c := NewDashscope(cfg, nil)
	assert.Equal(t, "https://custom.example/v1", c.BaseURL)
}

func TestDashscopeClientUsesDefaultHTTPWhenNil(t *testing.T) {
	t.Parallel()
	c := NewDashscope(vlmDashscopeConfig("qwen-max", "k"), nil)
	assert.NotNil(t, c.HTTP)
	// The Doer should be an *http.Client (the default).
	_, ok := c.HTTP.(*http.Client)
	assert.True(t, ok)
}
