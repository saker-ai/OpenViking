package vlm

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/saker-ai/ctxhub/internal/config"
)

// TestCodexClientDefaultsBaseURL verifies that NewCodex uses the OpenAI
// ChatGPT API endpoint by default.
func TestCodexClientDefaultsBaseURL(t *testing.T) {
	t.Parallel()
	c := NewCodex(config.VLMConfig{Model: "gpt-4", APIKey: "sk-x"}, nil)
	assert.Equal(t, DefaultCodexBaseURL, c.BaseURL)
	assert.Equal(t, "gpt-4", c.Model)
	assert.Equal(t, "sk-x", c.APIKey)
}

// TestCodexClientHonorsAPIBase verifies that a custom api_base overrides
// the default.
func TestCodexClientHonorsAPIBase(t *testing.T) {
	t.Parallel()
	cfg := config.VLMConfig{Model: "gpt-4", APIKey: "k", APIBase: "https://custom.example/v1/"}
	c := NewCodex(cfg, nil)
	assert.Equal(t, "https://custom.example/v1", c.BaseURL)
}

// TestGLMClientDefaultsBaseURL verifies that NewGLM uses the Zhipu
// BigModel endpoint by default.
func TestGLMClientDefaultsBaseURL(t *testing.T) {
	t.Parallel()
	c := NewGLM(config.VLMConfig{Model: "glm-4-plus", APIKey: "zhipu-key"}, nil)
	assert.Equal(t, DefaultGLMBaseURL, c.BaseURL)
	assert.Equal(t, "glm-4-plus", c.Model)
	assert.Equal(t, "zhipu-key", c.APIKey)
}

// TestGLMClientHonorsAPIBase verifies that a custom api_base overrides
// the Zhipu default.
func TestGLMClientHonorsAPIBase(t *testing.T) {
	t.Parallel()
	cfg := config.VLMConfig{Model: "glm-4-plus", APIKey: "k", APIBase: "https://glmirror.example/v4/"}
	c := NewGLM(cfg, nil)
	assert.Equal(t, "https://glmirror.example/v4", c.BaseURL)
}

// TestKimiClientDefaultsBaseURL verifies that NewKimi uses the Moonshot
// endpoint by default.
func TestKimiClientDefaultsBaseURL(t *testing.T) {
	t.Parallel()
	c := NewKimi(config.VLMConfig{Model: "moonshot-v1-32k", APIKey: "moon-key"}, nil)
	assert.Equal(t, DefaultKimiBaseURL, c.BaseURL)
	assert.Equal(t, "moonshot-v1-32k", c.Model)
	assert.Equal(t, "moon-key", c.APIKey)
}

// TestKimiClientHonorsAPIBase verifies that a custom api_base overrides
// the Moonshot default.
func TestKimiClientHonorsAPIBase(t *testing.T) {
	t.Parallel()
	cfg := config.VLMConfig{Model: "moonshot-v1-32k", APIKey: "k", APIBase: "https://proxy.example/v1/"}
	c := NewKimi(cfg, nil)
	assert.Equal(t, "https://proxy.example/v1", c.BaseURL)
}

// TestLiteLLMClientDefaultsBaseURL verifies that NewLiteLLM uses the
// local-proxy default.
func TestLiteLLMClientDefaultsBaseURL(t *testing.T) {
	t.Parallel()
	c := NewLiteLLM(config.VLMConfig{Model: "gpt-4", APIKey: "litellm-key"}, nil)
	assert.Equal(t, DefaultLiteLLMBaseURL, c.BaseURL)
	assert.Equal(t, "gpt-4", c.Model)
	assert.Equal(t, "litellm-key", c.APIKey)
}

// TestLiteLLMClientHonorsAPIBase verifies that a custom api_base overrides
// the local default.
func TestLiteLLMClientHonorsAPIBase(t *testing.T) {
	t.Parallel()
	cfg := config.VLMConfig{Model: "gpt-4", APIKey: "k", APIBase: "https://litellm.example/v1/"}
	c := NewLiteLLM(cfg, nil)
	assert.Equal(t, "https://litellm.example/v1", c.BaseURL)
}

// TestAllNewProvidersUseDefaultHTTPWhenNil verifies that all four new
// providers fall back to a non-nil HTTP client when nil is passed, so
// the first Chat call does not panic on a nil dereference.
func TestAllNewProvidersUseDefaultHTTPWhenNil(t *testing.T) {
	t.Parallel()
	cfg := config.VLMConfig{Model: "m", APIKey: "k"}
	clients := []*OpenAIClient{
		NewCodex(cfg, nil),
		NewGLM(cfg, nil),
		NewKimi(cfg, nil),
		NewLiteLLM(cfg, nil),
	}
	for i, c := range clients {
		assert.NotNilf(t, c.HTTP, "client %d: HTTP must not be nil", i)
		_, ok := c.HTTP.(*http.Client)
		assert.Truef(t, ok, "client %d: HTTP must be *http.Client, got %T", i, c.HTTP)
	}
}
