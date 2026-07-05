// Package embedder — Alibaba Cloud DashScope (Tongyi) embeddings client.
//
// DashScope exposes an OpenAI-compatible endpoint at
// https://dashscope.aliyuncs.com/compatible-mode/v1 — Alibaba recommends
// the compatible-mode endpoint for new integrations. We therefore reuse the
// OpenAIClient with a different default base URL and dimension table that
// recognizes the Tongyi text-embedding-v* family (1024 dims by default;
// text-embedding-v3 may be truncated to 768 or extended to 1536 via cfg.Dim).
//
// Tests inject an httptest.Server; no network calls.
package embedder

import (
	"net/http"

	"github.com/saker-ai/ctxhub/internal/config"
)

// DefaultDashscopeBaseURL is the DashScope OpenAI-compatible endpoint.
const DefaultDashscopeBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"

// NewDashscope constructs a DashScope-backed embedder. httpCLI may be nil.
//
// The compatible-mode endpoint accepts the OpenAI /embeddings wire shape
// verbatim; model ids are the DashScope names (text-embedding-v3,
// text-embedding-v4, etc.). When cfg.Dim > 0 it overrides the default
// dimension for the configured model so callers can pin v3 to 768/1536.
func NewDashscope(cfg config.EmbedderConfig, httpCLI *http.Client) *OpenAIClient {
	base := cfg.APIBase
	if base == "" {
		base = DefaultDashscopeBaseURL
	}
	c := NewOpenAI(base, cfg.APIKey, httpCLI)
	if cfg.Dim > 0 {
		c.DimByModel = map[string]int{cfg.Model: cfg.Dim}
	}
	return c
}
