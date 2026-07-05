package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/saker-ai/ctxhub/internal/bot/channels"
	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/bot/observability"
	"github.com/saker-ai/ctxhub/internal/bot/providers"
	"github.com/saker-ai/ctxhub/internal/bot/session"
	"github.com/saker-ai/ctxhub/internal/domain"
	isession "github.com/saker-ai/ctxhub/internal/session"
)

type stubMCP struct {
	tools  []providers.ToolDescription
	calls  atomic.Int32
	last   map[string]any
	failOn string
}

func (s *stubMCP) ListTools(ctx context.Context) ([]providers.ToolDescription, error) {
	return s.tools, nil
}

func (s *stubMCP) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	s.calls.Add(1)
	s.last = args
	if s.failOn == name {
		return "", errors.New("tool error")
	}
	return "result-" + name, nil
}

func newTestAgent(t *testing.T, p providers.Provider, mcp MCPClient) (*Agent, *session.Adapter) {
	t.Helper()
	store := isession.NewMemoryStore(isession.StoreConfig{})
	sess := session.New(store)
	obs := observability.New(config.OTELConfig{ServiceName: "vikingbot"})
	cfg := config.ProviderConfig{SystemPrompt: "sys", MaxIterations: 3}
	return New(cfg, p, mcp, sess, obs), sess
}

func TestAgent_HandleNoProvider(t *testing.T) {
	a, _ := newTestAgent(t, nil, nil)
	_, err := a.Handle(context.Background(), channels.IncomingMessage{Text: "hi"})
	if err == nil {
		t.Fatalf("Handle should error when provider is nil")
	}
}

func TestAgent_HandleEmptyText(t *testing.T) {
	p := providers.NewStub(config.ProviderConfig{})
	a, _ := newTestAgent(t, p, nil)
	out, err := a.Handle(context.Background(), channels.IncomingMessage{Text: ""})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out != "" {
		t.Errorf("out = %q, want empty", out)
	}
}

func TestAgent_HandleChatOnly(t *testing.T) {
	p := providers.NewStub(config.ProviderConfig{SystemPrompt: "sys"})
	a, _ := newTestAgent(t, p, nil)
	msg := channels.IncomingMessage{
		Text:        "hello",
		ChannelName: "telegram",
		UserID:      "u1",
		Identity:    domain.Identifier{Account: "acct", User: "u1", ActorPeer: "telegram"},
	}
	out, err := a.Handle(context.Background(), msg)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out != "sys\n\nhello" {
		t.Errorf("out = %q", out)
	}
}

func TestAgent_HandleWithToolCall(t *testing.T) {
	// Stub provider that issues a tool call on first Chat, then
	// returns a final text reply on the second.
	calls := atomic.Int32{}
	p := &providers.StubProvider{}
	p.OnChat = func(ctx context.Context, msgs []providers.Message, _ []providers.ToolDescription) (*providers.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			return &providers.Response{
				ToolCalls: []providers.ToolCall{
					{ID: "call-1", Name: "find", Args: map[string]any{"q": "x"}},
				},
				FinishReason: "tool_calls",
			}, nil
		}
		return &providers.Response{Content: "final answer", FinishReason: "stop"}, nil
	}
	mcp := &stubMCP{tools: []providers.ToolDescription{{Name: "find", Description: "find a resource"}}}
	a, _ := newTestAgent(t, p, mcp)
	msg := channels.IncomingMessage{
		Text:        "find x",
		ChannelName: "telegram",
		UserID:      "u1",
		Identity:    domain.Identifier{Account: "acct", User: "u1", ActorPeer: "telegram"},
	}
	out, err := a.Handle(context.Background(), msg)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out != "final answer" {
		t.Errorf("out = %q, want 'final answer'", out)
	}
	if mcp.calls.Load() != 1 {
		t.Errorf("MCP calls = %d, want 1", mcp.calls.Load())
	}
	if mcp.last["q"] != "x" {
		t.Errorf("MCP args = %+v", mcp.last)
	}
}

func TestAgent_HandleToolErrorHandled(t *testing.T) {
	calls := atomic.Int32{}
	p := &providers.StubProvider{}
	p.OnChat = func(ctx context.Context, _ []providers.Message, _ []providers.ToolDescription) (*providers.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			return &providers.Response{
				ToolCalls:    []providers.ToolCall{{ID: "c1", Name: "broken", Args: map[string]any{}}},
				FinishReason: "tool_calls",
			}, nil
		}
		return &providers.Response{Content: "fallback", FinishReason: "stop"}, nil
	}
	mcp := &stubMCP{failOn: "broken"}
	a, _ := newTestAgent(t, p, mcp)
	msg := channels.IncomingMessage{
		Text: "x", ChannelName: "telegram", UserID: "u",
		Identity: domain.Identifier{Account: "acct", User: "u", ActorPeer: "telegram"},
	}
	out, err := a.Handle(context.Background(), msg)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out != "fallback" {
		t.Errorf("out = %q, want 'fallback'", out)
	}
}

func TestAgent_BuildConversationIncludesSystemPrompt(t *testing.T) {
	p := providers.NewStub(config.ProviderConfig{SystemPrompt: "sys"})
	a, _ := newTestAgent(t, p, nil)
	msg := channels.IncomingMessage{
		Text: "hi", ChannelName: "telegram", UserID: "u",
		Identity: domain.Identifier{Account: "acct", User: "u", ActorPeer: "telegram"},
	}
	sessionID, _ := a.sessions.EnsureSession(context.Background(), msg)
	conv := a.buildConversation(context.Background(), msg, sessionID)
	if len(conv) == 0 || conv[0].Role != providers.RoleSystem || conv[0].Content != "sys" {
		t.Errorf("first message = %+v", conv[0])
	}
	// Last should be the user message.
	last := conv[len(conv)-1]
	if last.Role != providers.RoleUser || last.Content != "hi" {
		t.Errorf("last message = %+v", last)
	}
}
