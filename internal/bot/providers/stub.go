package providers

import (
	"context"
	"sync"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// StubProvider is a deterministic Provider for tests. It returns a
// canned Response per-call (set via OnChat) or a default echo of the
// last user message. Safe for concurrent use.
type StubProvider struct {
	mu      sync.Mutex
	cfg     config.ProviderConfig
	OnChat  func(ctx context.Context, messages []Message, tools []ToolDescription) (*Response, error)
	calls   int
	lastMsg []Message
}

// NewStub returns a StubProvider with a default echo behavior.
func NewStub(cfg config.ProviderConfig) *StubProvider {
	return &StubProvider{
		cfg: cfg,
		OnChat: func(ctx context.Context, msgs []Message, _ []ToolDescription) (*Response, error) {
			// Echo the last user message back, prefixed with the
			// configured system prompt when present.
			last := ""
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role == RoleUser {
					last = msgs[i].Content
					break
				}
			}
			prefix := ""
			if cfg.SystemPrompt != "" {
				prefix = cfg.SystemPrompt + "\n\n"
			}
			return &Response{Content: prefix + last, FinishReason: "stop"}, nil
		},
	}
}

// Chat implements Provider.
func (s *StubProvider) Chat(ctx context.Context, messages []Message, tools []ToolDescription) (*Response, error) {
	s.mu.Lock()
	s.calls++
	s.lastMsg = messages
	fn := s.OnChat
	s.mu.Unlock()
	if fn == nil {
		return nil, ErrEmptyResponse
	}
	return fn(ctx, messages, tools)
}

// Calls returns the number of Chat invocations observed. Useful in tests.
func (s *StubProvider) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// LastMessages returns the most recent message slice handed to Chat.
func (s *StubProvider) LastMessages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastMsg
}
