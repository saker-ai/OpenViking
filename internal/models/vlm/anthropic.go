// Package vlm — Anthropic Claude chat client built on the official SDK.
//
// Anthropic's Messages API differs enough from OpenAI's chat completions
// (top-level "system", different tool schema, x-api-key header, version
// pinning) to warrant a separate client. We delegate the wire format,
// retry, and SSE plumbing to github.com/anthropics/anthropic-sdk-go and
// only adapt the provider-agnostic ChatRequest/ChatResponse shapes onto
// the SDK's typed params. Embeddings are not offered on the public
// Anthropic API; Embed returns domain.ErrUnsupported so callers route
// embedding traffic through a dedicated embedder.Embedder.
//
// Tests inject an httptest.Server as the SDK's base URL + HTTP client;
// no network calls.
package vlm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// anthropicVersion is the API version header value the SDK pins
// automatically (see requestconfig.go). Exposed as a constant so tests
// can assert against the literal value without importing SDK internals.
const anthropicVersion = "2023-06-01"

// AnthropicClient is a VLM backed by Anthropic's Messages API.
type AnthropicClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
	cli     anthropic.Client
}

// NewAnthropic constructs an Anthropic-backed VLM.
//
// httpCLI may be nil; the default is a 30s-timeout *http.Client. The
// HTTP client and base URL are forwarded to the SDK via
// option.WithHTTPClient / option.WithBaseURL so tests can substitute
// an httptest.Server recorder.
func NewAnthropic(baseURL, apiKey, model string, httpCLI *http.Client) *AnthropicClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	// The SDK joins relative paths (e.g. "v1/messages") onto BaseURL,
	// so it must end with a slash. Trim any user-supplied trailing slash
	// first to avoid a double-slash.
	canonical := strings.TrimRight(baseURL, "/") + "/"
	cli := anthropic.NewClient(
		// Skip ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL env autoload so
		// constructor arguments remain the single source of truth and
		// test runs do not leak local credentials into the request.
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(apiKey),
		option.WithBaseURL(canonical),
		option.WithHTTPClient(httpCLI),
	)
	return &AnthropicClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    httpCLI,
		cli:     cli,
	}
}

// Chat calls POST {BaseURL}/v1/messages via the SDK.
func (c *AnthropicClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = c.Model
	}
	if model == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vlm.anthropic: model is required"))
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1024
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(req.MaxTokens),
	}
	if req.Temperature > 0 {
		params.Temperature = param.NewOpt(req.Temperature)
	}
	if req.TopP > 0 {
		params.TopP = param.NewOpt(req.TopP)
	}
	if len(req.Stop) > 0 {
		params.StopSequences = req.Stop
	}

	var systemText strings.Builder
	for _, m := range req.Messages {
		if m.Role != RoleSystem {
			continue
		}
		if systemText.Len() > 0 {
			systemText.WriteString("\n\n")
		}
		systemText.WriteString(m.Content)
	}
	if s := systemText.String(); s != "" {
		params.System = []anthropic.TextBlockParam{{Text: s}}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			// already folded into params.System
		case RoleAssistant:
			params.Messages = append(params.Messages,
				anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		case RoleUser, RoleTool:
			// Anthropic has no "tool" role; tool results are sent as
			// user messages with tool_result content blocks. For now
			// preserve the prior behavior of mapping tool→user with
			// plain text content; tool_result blocks will land in a
			// later phase.
			params.Messages = append(params.Messages,
				anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		}
	}

	for _, t := range req.Tools {
		schema := anthropic.ToolInputSchemaParam{ExtraFields: t.Schema}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				InputSchema: schema,
			},
		})
	}

	msg, err := c.cli.Messages.New(ctx, params)
	if err != nil {
		// The SDK's *anthropic.Error already formats a useful message
		// ("POST \"<url>\": 401 Unauthorized ..."); wrap it with the
		// VLM business code so callers can errors.Is against
		// domain.ErrVLMFailed.
		return nil, wrapVLM(err)
	}

	out := &ChatResponse{
		Model:        string(msg.Model),
		FinishReason: mapAnthropicStop(string(msg.StopReason)),
		Usage: Usage{
			PromptTokens:     int(msg.Usage.InputTokens),
			CompletionTokens: int(msg.Usage.OutputTokens),
			TotalTokens:      int(msg.Usage.InputTokens + msg.Usage.OutputTokens),
		},
	}
	var buf strings.Builder
	for _, blk := range msg.Content {
		switch blk.Type {
		case "text":
			buf.WriteString(blk.Text)
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:        blk.ID,
				Name:      blk.Name,
				Arguments: string(blk.Input),
			})
		}
	}
	out.Content = buf.String()
	return out, nil
}

// Embed returns domain.ErrUnsupported — Anthropic does not expose a public
// embeddings endpoint. Use a dedicated embedder.Embedder instead.
func (c *AnthropicClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, domain.ErrUnsupported
}

// mapAnthropicStop translates the SDK's StopReason enum into the
// OpenAI-aligned FinishReason values that the rest of the VLM layer
// expects. Unknown values pass through unchanged.
func mapAnthropicStop(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence":
		return "stop"
	default:
		return reason
	}
}
