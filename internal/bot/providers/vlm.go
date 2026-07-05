package providers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
)

// VLMProvider is a Provider that delegates to a vlm.VLM client (built
// in P7). It adapts the providers.Message / providers.ToolDescription
// surface used by the agent loop to the vlm.ChatRequest surface used
// by the underlying model clients (OpenAI, Anthropic, Volcengine).
//
// The VLM client is injected via NewVLM so callers can substitute a
// vlm.Stub in tests. Production code uses NewFromConfig to construct
// a real vlm.OpenAIClient / vlm.AnthropicClient from the bot config.
type VLMProvider struct {
	cfg  config.ProviderConfig
	vlm  vlm.VLM
}

// NewVLM returns a VLMProvider backed by the supplied VLM client. The
// client must be non-nil; pass vlm.NewStub() for tests.
func NewVLM(cfg config.ProviderConfig, client vlm.VLM) *VLMProvider {
	if client == nil {
		client = vlm.NewStub()
	}
	return &VLMProvider{cfg: cfg, vlm: client}
}

// NewVLMFromConfig constructs a VLMProvider from a bot config. The
// provider backend is selected by cfg.BaseURL: when it contains
// "anthropic.com" we use vlm.NewAnthropic; otherwise we use
// vlm.NewOpenAI (which also serves Volcengine Ark via /api/v3).
//
// When the bot config has no BaseURL we return a clear error so
// callers know they need to configure one — silent fallback to a
// stub provider would mask the missing configuration in production.
func NewVLMFromConfig(cfg config.ProviderConfig) (*VLMProvider, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("providers: vlm backend requires base_url")
	}
	var client vlm.VLM
	switch {
	case contains(cfg.BaseURL, "anthropic.com"):
		client = vlm.NewAnthropic(cfg.BaseURL, cfg.APIKey, cfg.Model, nil)
	default:
		// OpenAI-compatible (OpenAI, Azure OpenAI, Volcengine Ark,
		// LiteLLM, local proxies).
		client = vlm.NewOpenAI(cfg.BaseURL, cfg.APIKey, cfg.Model, nil)
	}
	return NewVLM(cfg, client), nil
}

// Chat implements Provider by delegating to the underlying VLM client.
func (p *VLMProvider) Chat(ctx context.Context, messages []Message, tools []ToolDescription) (*Response, error) {
	req := vlm.ChatRequest{
		Model:    p.cfg.Model,
		Messages: toVLMMessages(messages),
		Tools:    toVLMTools(tools),
	}
	if p.cfg.SystemPrompt != "" {
		// Prepend the system prompt as a system message; vlm clients
		// that map system to a top-level parameter (Anthropic) handle
		// this in their own Chat method.
		req.Messages = append([]vlm.Message{
			{Role: vlm.RoleSystem, Content: p.cfg.SystemPrompt},
		}, req.Messages...)
	}
	resp, err := p.vlm.Chat(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("providers: vlm chat: %w", err)
	}
	out := &Response{
		Content:      resp.Content,
		ToolCalls:    fromVLMToolCalls(resp.ToolCalls),
		FinishReason: resp.FinishReason,
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return nil, ErrEmptyResponse
	}
	return out, nil
}

// toVLMMessages converts providers.Message -> vlm.Message.
func toVLMMessages(msgs []Message) []vlm.Message {
	out := make([]vlm.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, vlm.Message{
			Role:       vlm.Role(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			ToolCalls:  toVLMToolCalls(m.ToolCalls),
		})
	}
	return out
}

// toVLMTools converts []ToolDescription -> []vlm.Tool.
func toVLMTools(tools []ToolDescription) []vlm.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]vlm.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, vlm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Parameters,
		})
	}
	return out
}

// toVLMToolCalls converts []ToolCall -> []vlm.ToolCall. The providers
// surface carries args as a decoded map; vlm carries them as a JSON
// string. We re-encode.
func toVLMToolCalls(calls []ToolCall) []vlm.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]vlm.ToolCall, 0, len(calls))
	for _, c := range calls {
		args, _ := json.Marshal(c.Args)
		out = append(out, vlm.ToolCall{
			ID:        c.ID,
			Name:      c.Name,
			Arguments: string(args),
		})
	}
	return out
}

// fromVLMToolCalls converts []vlm.ToolCall -> []ToolCall. The vlm
// surface carries args as a JSON string; we decode it into a map.
func fromVLMToolCalls(calls []vlm.ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		args := map[string]any{}
		if c.Arguments != "" {
			_ = json.Unmarshal([]byte(c.Arguments), &args)
		}
		out = append(out, ToolCall{
			ID:   c.ID,
			Name: c.Name,
			Args: args,
		})
	}
	return out
}

// contains is a small substring helper.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Compile-time assertion that VLMProvider satisfies Provider.
var _ Provider = (*VLMProvider)(nil)
