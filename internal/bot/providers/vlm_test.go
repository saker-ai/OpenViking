package providers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
)

// TestVLMProvider_ChatDelegates verifies that VLMProvider.Chat forwards
// the conversation to the underlying vlm.VLM client and maps the
// response back faithfully.
func TestVLMProvider_ChatDelegates(t *testing.T) {
	stub := vlm.NewStub()
	stub.ChatFn = func(ctx context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error) {
		// Assert the system prompt is prepended.
		if len(req.Messages) < 2 {
			t.Errorf("expected system+user, got %d messages", len(req.Messages))
		}
		if req.Messages[0].Role != vlm.RoleSystem || req.Messages[0].Content != "sys" {
			t.Errorf("system message = %+v", req.Messages[0])
		}
		if req.Messages[1].Content != "hi" {
			t.Errorf("user content = %q", req.Messages[1].Content)
		}
		return &vlm.ChatResponse{
			Content:      "hello back",
			FinishReason: "stop",
		}, nil
	}
	p := NewVLM(config.ProviderConfig{Backend: "vlm", Model: "m", SystemPrompt: "sys"}, stub)
	resp, err := p.Chat(context.Background(), []Message{
		{Role: RoleUser, Content: "hi"},
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hello back" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
}

// TestVLMProvider_ChatToolCalls verifies that tool calls from the VLM
// are decoded into the providers.ToolCall shape (args decoded from
// JSON string to map).
func TestVLMProvider_ChatToolCalls(t *testing.T) {
	stub := vlm.NewStub()
	stub.ChatFn = func(ctx context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error) {
		return &vlm.ChatResponse{
			ToolCalls: []vlm.ToolCall{
				{ID: "c1", Name: "find", Arguments: `{"q":"x"}`},
			},
			FinishReason: "tool_calls",
		}, nil
	}
	p := NewVLM(config.ProviderConfig{Backend: "vlm", Model: "m"}, stub)
	resp, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "q"}}, []ToolDescription{
		{Name: "find", Description: "find a resource"},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "c1" || tc.Name != "find" || tc.Args["q"] != "x" {
		t.Errorf("ToolCall = %+v", tc)
	}
}

// TestVLMProvider_ChatEmptyResponse verifies that an empty content + no
// tool calls surfaces ErrEmptyResponse.
func TestVLMProvider_ChatEmptyResponse(t *testing.T) {
	stub := vlm.NewStub()
	stub.ChatFn = func(ctx context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error) {
		return &vlm.ChatResponse{}, nil
	}
	p := NewVLM(config.ProviderConfig{Backend: "vlm", Model: "m"}, stub)
	_, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "q"}}, nil)
	if !errors.Is(err, ErrEmptyResponse) {
		t.Errorf("err = %v, want ErrEmptyResponse", err)
	}
}

// TestVLMProvider_ChatVLMError verifies that errors from the underlying
// VLM client are wrapped faithfully.
func TestVLMProvider_ChatVLMError(t *testing.T) {
	stub := vlm.NewStub()
	stub.ChatFn = func(ctx context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error) {
		return nil, errors.New("upstream 503")
	}
	p := NewVLM(config.ProviderConfig{Backend: "vlm", Model: "m"}, stub)
	_, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "q"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "upstream 503") {
		t.Errorf("err = %v, want 'upstream 503'", err)
	}
}

// TestVLMProvider_ToolsAdapted verifies that ToolDescription is
// converted to vlm.Tool with the right Name / Description / Schema.
func TestVLMProvider_ToolsAdapted(t *testing.T) {
	var gotTools []vlm.Tool
	stub := vlm.NewStub()
	stub.ChatFn = func(ctx context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error) {
		gotTools = req.Tools
		return &vlm.ChatResponse{Content: "ok", FinishReason: "stop"}, nil
	}
	p := NewVLM(config.ProviderConfig{Backend: "vlm", Model: "m"}, stub)
	_, _ = p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "q"}}, []ToolDescription{
		{Name: "find", Description: "find a resource", Parameters: map[string]any{"type": "object"}},
		{Name: "read", Description: "read a file"},
	})
	if len(gotTools) != 2 {
		t.Fatalf("gotTools = %+v", gotTools)
	}
	if gotTools[0].Name != "find" || gotTools[0].Description != "find a resource" {
		t.Errorf("tool[0] = %+v", gotTools[0])
	}
	if gotTools[0].Schema["type"] != "object" {
		t.Errorf("tool[0] schema = %+v", gotTools[0].Schema)
	}
	if gotTools[1].Name != "read" {
		t.Errorf("tool[1] = %+v", gotTools[1])
	}
}

// TestNewVLMFromConfig_OpenAI verifies that NewVLMFromConfig builds an
// OpenAI-compatible client when BaseURL is not Anthropic.
func TestNewVLMFromConfig_OpenAI(t *testing.T) {
	p, err := NewVLMFromConfig(config.ProviderConfig{
		Backend: "vlm",
		BaseURL: "https://api.openai.com/v1",
		APIKey:  "sk-test",
		Model:   "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("NewVLMFromConfig: %v", err)
	}
	if _, ok := p.vlm.(*vlm.OpenAIClient); !ok {
		t.Errorf("vlm = %T, want *vlm.OpenAIClient", p.vlm)
	}
}

// TestNewVLMFromConfig_Anthropic verifies that NewVLMFromConfig builds
// an Anthropic client when BaseURL contains "anthropic.com".
func TestNewVLMFromConfig_Anthropic(t *testing.T) {
	p, err := NewVLMFromConfig(config.ProviderConfig{
		Backend: "vlm",
		BaseURL: "https://api.anthropic.com",
		APIKey:  "sk-test",
		Model:   "claude-3-5-sonnet",
	})
	if err != nil {
		t.Fatalf("NewVLMFromConfig: %v", err)
	}
	if _, ok := p.vlm.(*vlm.AnthropicClient); !ok {
		t.Errorf("vlm = %T, want *vlm.AnthropicClient", p.vlm)
	}
}

// TestNewVLMFromConfig_NoBaseURL verifies that a missing BaseURL
// surfaces a clear error.
func TestNewVLMFromConfig_NoBaseURL(t *testing.T) {
	_, err := NewVLMFromConfig(config.ProviderConfig{Backend: "vlm"})
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("err = %v, want 'base_url'", err)
	}
}
