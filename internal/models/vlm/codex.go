// Package vlm — OpenAI Codex chat client.
//
// "codex" in OpenViking's provider enum is an alias for the OpenAI
// ChatGPT API at https://api.openai.com/v1 — it exists so that users
// migrating from the Python openviking vlm Provider enum can use
// `provider: codex` without surprises. The Codex-specific model ids
// (e.g. "gpt-4", "gpt-4-turbo", "gpt-4o") are passed verbatim.
//
// Auth: Bearer <OPENAI_API_KEY>.
//
// Tests inject an httptest.Server; no network calls.
package vlm

import (
	"net/http"

	"github.com/saker-ai/ctxhub/internal/config"
)

// DefaultCodexBaseURL is the OpenAI ChatGPT API endpoint. May be
// overridden via VLMConfig.APIBase.
const DefaultCodexBaseURL = "https://api.openai.com/v1"

// NewCodex constructs an OpenAI-backed VLM from a VLMConfig using the
// Codex default base URL. The returned client is a thin wrapper around
// OpenAIClient; behavior is identical to NewOpenAI except that the
// constructor signature matches the dashscope/glm/kimi/litellm shape
// (takes a VLMConfig) so the dispatcher can route uniformly.
//
// httpCLI may be nil; the default is a 30s-timeout *http.Client.
func NewCodex(cfg config.VLMConfig, httpCLI *http.Client) *OpenAIClient {
	base := cfg.APIBase
	if base == "" {
		base = DefaultCodexBaseURL
	}
	return &OpenAIClient{
		BaseURL: trimRight(base),
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
		HTTP:    orDefault(httpCLI),
	}
}
