package doctor

import (
	"fmt"
	"net/http"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/models/rerank"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
)

// newEmbedderClient builds an embedder.Embedder for the configured provider.
// Providers not yet implemented return an error so the probe reports fail
// rather than silently skipping.
func newEmbedderClient(cfg config.EmbedderConfig, httpCLI *http.Client) (embedder.Embedder, error) {
	switch cfg.Provider {
	case "openai", "litellm":
		return embedder.NewOpenAI(cfg.APIBase, cfg.APIKey, httpCLI), nil
	case "volcengine":
		return embedder.NewVolcengine(cfg, httpCLI), nil
	case "dashscope":
		return embedder.NewDashscope(cfg, httpCLI), nil
	case "local":
		return embedder.NewLocal(cfg, httpCLI), nil
	default:
		return nil, fmt.Errorf("embedder: provider %q not probeable", cfg.Provider)
	}
}

// newRerankerClient builds a rerank.Reranker for the configured provider.
func newRerankerClient(cfg config.RerankConfig, httpCLI *http.Client) (rerank.Reranker, error) {
	switch cfg.Provider {
	case "cohere":
		return rerank.NewCohere(cfg.APIBase, cfg.APIKey, cfg.Model, httpCLI), nil
	case "dashscope":
		return rerank.NewDashscope(cfg.APIBase, cfg.APIKey, cfg.Model, httpCLI), nil
	case "volcengine":
		return rerank.NewVolcengine(cfg.APIBase, cfg.APIKey, cfg.Model, httpCLI), nil
	case "local":
		return rerank.NewLocal(), nil
	default:
		return nil, fmt.Errorf("rerank: provider %q not probeable", cfg.Provider)
	}
}

// newVLMClient builds a vlm.VLM for the configured provider.
func newVLMClient(cfg config.VLMConfig, httpCLI *http.Client) (vlm.VLM, error) {
	switch cfg.Provider {
	case "openai":
		return vlm.NewOpenAI(cfg.APIBase, cfg.APIKey, cfg.Model, httpCLI), nil
	case "codex":
		return vlm.NewCodex(cfg, httpCLI), nil
	case "glm":
		return vlm.NewGLM(cfg, httpCLI), nil
	case "kimi":
		return vlm.NewKimi(cfg, httpCLI), nil
	case "litellm":
		return vlm.NewLiteLLM(cfg, httpCLI), nil
	case "volcengine":
		return vlm.NewVolcengine(cfg, httpCLI), nil
	case "dashscope":
		return vlm.NewDashscope(cfg, httpCLI), nil
	case "anthropic":
		return vlm.NewAnthropic(cfg.APIBase, cfg.APIKey, cfg.Model, httpCLI), nil
	default:
		return nil, fmt.Errorf("vlm: provider %q not probeable", cfg.Provider)
	}
}
