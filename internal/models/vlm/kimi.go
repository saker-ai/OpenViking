// Package vlm — Moonshot AI (Kimi) chat client.
//
// Moonshot exposes an OpenAI-compatible endpoint at
// https://api.moonshot.cn/v1 — the Kimi family (moonshot-v1-8k,
// moonshot-v1-32k, moonshot-v1-128k, kimi-k1-...) is reachable via the
// same /chat/completions path as OpenAI. We therefore reuse the
// OpenAIClient with a different default base URL and pass the Kimi model
// id verbatim.
//
// Auth: Bearer <MOONSHOT_API_KEY>.
//
// Tests inject an httptest.Server; no network calls.
package vlm

import (
	"net/http"

	"github.com/saker-ai/ctxhub/internal/config"
)

// DefaultKimiBaseURL is the Moonshot OpenAI-compatible endpoint. May be
// overridden via VLMConfig.APIBase.
const DefaultKimiBaseURL = "https://api.moonshot.cn/v1"

// NewKimi constructs a Moonshot Kimi-backed VLM from a VLMConfig. The
// returned client is a thin wrapper around OpenAIClient; behavior is
// identical to NewOpenAI except for the default base URL.
//
// httpCLI may be nil; the default is a 30s-timeout *http.Client.
func NewKimi(cfg config.VLMConfig, httpCLI *http.Client) *OpenAIClient {
	base := cfg.APIBase
	if base == "" {
		base = DefaultKimiBaseURL
	}
	return &OpenAIClient{
		BaseURL: trimRight(base),
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
		HTTP:    orDefault(httpCLI),
	}
}
