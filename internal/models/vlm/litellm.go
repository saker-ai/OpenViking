// Package vlm — LiteLLM proxy chat client.
//
// LiteLLM (https://github.com/BerriAI/litellm) is a unified proxy that
// exposes OpenAI-compatible /chat/completions and /embeddings endpoints
// for 100+ underlying LLM providers. The default proxy URL is
// http://localhost:4000/v1 (the LiteLLM docker default). We therefore
// reuse the OpenAIClient with the LiteLLM default base URL.
//
// Auth: Bearer <LITELLM_API_KEY> (the proxy's configured master key, or
// the upstream provider key when the proxy is in pass-through mode).
//
// Tests inject an httptest.Server; no network calls.
package vlm

import (
	"net/http"

	"github.com/saker-ai/ctxhub/internal/config"
)

// DefaultLiteLLMBaseURL is the LiteLLM proxy OpenAI-compatible endpoint.
// May be overridden via VLMConfig.APIBase.
const DefaultLiteLLMBaseURL = "http://localhost:4000/v1"

// NewLiteLLM constructs a LiteLLM-proxy-backed VLM from a VLMConfig. The
// returned client is a thin wrapper around OpenAIClient; behavior is
// identical to NewOpenAI except for the default base URL.
//
// httpCLI may be nil; the default is a 30s-timeout *http.Client.
func NewLiteLLM(cfg config.VLMConfig, httpCLI *http.Client) *OpenAIClient {
	base := cfg.APIBase
	if base == "" {
		base = DefaultLiteLLMBaseURL
	}
	return &OpenAIClient{
		BaseURL: trimRight(base),
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
		HTTP:    orDefault(httpCLI),
	}
}
