// Package vlm — Alibaba Cloud DashScope (Tongyi Qwen) chat client.
//
// DashScope exposes an OpenAI-compatible endpoint at
// https://dashscope.aliyuncs.com/compatible-mode/v1 — Alibaba recommends
// the compatible-mode endpoint for new integrations. We therefore reuse the
// OpenAIClient with a different default base URL and model id convention
// (Qwen endpoints are passed as "model": qwen-max / qwen-plus / qwen-turbo /
// qwen2.5-* etc.).
//
// Tests inject an httptest.Server; no network calls.
package vlm

import (
	"net/http"

	"github.com/saker-ai/ctxhub/internal/config"
)

// DefaultDashscopeBaseURL is the DashScope OpenAI-compatible endpoint. May
// be overridden via VLMConfig.APIBase.
const DefaultDashscopeBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"

// NewDashscope constructs a DashScope-backed VLM from a VLMConfig. The
// returned client is a thin wrapper around OpenAIClient; behavior is
// identical to NewOpenAI except for the default base URL.
//
// httpCLI may be nil; the default is a 30s-timeout *http.Client.
func NewDashscope(cfg config.VLMConfig, httpCLI *http.Client) *OpenAIClient {
	base := cfg.APIBase
	if base == "" {
		base = DefaultDashscopeBaseURL
	}
	return &OpenAIClient{
		BaseURL: trimRight(base),
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
		HTTP:    orDefault(httpCLI),
	}
}
