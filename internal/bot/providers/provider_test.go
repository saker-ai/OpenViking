package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

func TestNew_StubDefault(t *testing.T) {
	p, err := New(config.ProviderConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := p.(*StubProvider); !ok {
		t.Errorf("New() = %T, want *StubProvider", p)
	}
}

func TestNew_VLMRequiresBaseURL(t *testing.T) {
	_, err := New(config.ProviderConfig{Backend: "vlm"})
	if err == nil {
		t.Fatalf("New should error when vlm backend has no base_url")
	}
	if !strings.Contains(err.Error(), "base_url") {
		t.Errorf("err = %v, want mention of base_url", err)
	}
}

func TestNew_UnknownBackend(t *testing.T) {
	_, err := New(config.ProviderConfig{Backend: "bogus"})
	if err == nil {
		t.Fatalf("New should error on unknown backend")
	}
	if !strings.Contains(err.Error(), "unknown backend") {
		t.Errorf("err = %v, want 'unknown backend'", err)
	}
}

func TestStubProvider_Echo(t *testing.T) {
	p := NewStub(config.ProviderConfig{SystemPrompt: "sys"})
	resp, err := p.Chat(context.Background(), []Message{
		{Role: RoleUser, Content: "hello"},
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "sys\n\nhello" {
		t.Errorf("Content = %q, want sys\\n\\nhello", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if p.Calls() != 1 {
		t.Errorf("Calls = %d, want 1", p.Calls())
	}
}

func TestStubProvider_OnChatOverride(t *testing.T) {
	p := NewStub(config.ProviderConfig{})
	p.OnChat = func(ctx context.Context, msgs []Message, _ []ToolDescription) (*Response, error) {
		return &Response{ToolCalls: []ToolCall{{ID: "c1", Name: "find", Args: map[string]any{"q": "x"}}}, FinishReason: "tool_calls"}, nil
	}
	resp, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "q"}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "find" {
		t.Errorf("ToolCalls = %+v", resp.ToolCalls)
	}
}
