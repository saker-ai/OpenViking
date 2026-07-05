// Package vlm — Zhipu AI (BigModel) GLM chat client.
//
// Zhipu exposes an OpenAI-compatible endpoint at
// https://open.bigmodel.cn/api/paas/v4 — the GLM family (GLM-4-Plus,
// GLM-4-0520, GLM-4-Air, etc.) is reachable via the same /chat/completions
// and /embeddings paths as OpenAI. We therefore reuse the OpenAIClient
// with a different default base URL and pass the GLM model id verbatim
// (e.g. "glm-4-plus", "glm-4-flash").
//
// Auth: Bearer <ZHIPU_API_KEY>. The BigModel platform keys are formatted
// as "<id>.<secret>" but are sent as a single Bearer token.
//
// Tests inject an httptest.Server; no network calls.
package vlm

import (
	"net/http"

	"github.com/saker-ai/ctxhub/internal/config"
)

// DefaultGLMBaseURL is the Zhipu BigModel OpenAI-compatible endpoint. May
// be overridden via VLMConfig.APIBase.
const DefaultGLMBaseURL = "https://open.bigmodel.cn/api/paas/v4"

// NewGLM constructs a Zhipu GLM-backed VLM from a VLMConfig. The returned
// client is a thin wrapper around OpenAIClient; behavior is identical to
// NewOpenAI except for the default base URL.
//
// httpCLI may be nil; the default is a 30s-timeout *http.Client.
func NewGLM(cfg config.VLMConfig, httpCLI *http.Client) *OpenAIClient {
	base := cfg.APIBase
	if base == "" {
		base = DefaultGLMBaseURL
	}
	return &OpenAIClient{
		BaseURL: trimRight(base),
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
		HTTP:    orDefault(httpCLI),
	}
}
