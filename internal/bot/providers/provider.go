// Package providers abstracts the LLM provider used by the agent loop.
//
// The agent loop needs a single capability — given a list of messages
// and a list of tool descriptions, return either a final text response
// or a tool-call request. This package defines the Provider interface
// and concrete implementations:
//   - StubProvider: deterministic echo, used in tests.
//   - OpenAIProvider: direct HTTP client to an OpenAI-compatible
//     /chat/completions endpoint.
//   - VLMProvider: delegates to internal/models/vlm (P7's OpenAI /
//     Anthropic / Volcengine clients) so the bot can share the same
//     provider surface as the server pipelines.
//
// The bot does not import provider SDKs; all HTTP is performed through
// *http.Client so tests inject httptest.Server recorders.
package providers

import (
	"context"
	"errors"
	"fmt"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// Role enumerates the role of a message in a conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a single conversational turn sent to the provider.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// ToolCallID is set on RoleTool messages to associate the result
	// with the assistant's prior tool call.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls is set on RoleAssistant messages that request tool
	// execution. The provider fills this when it wants the agent loop
	// to invoke a tool.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is a single tool invocation request from the assistant.
type ToolCall struct {
	// ID is the assistant-assigned identifier used to correlate the
	// subsequent RoleTool result.
	ID string `json:"id"`
	// Name is the tool name (must match a ToolDescription.Name).
	Name string `json:"name"`
	// Args is the JSON-decoded argument map.
	Args map[string]any `json:"args,omitempty"`
}

// ToolDescription describes one tool the assistant may call.
type ToolDescription struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters is a JSON Schema object describing the tool args.
	Parameters map[string]any `json:"parameters,omitempty"`
}

// Response is the provider's reply. Either Content or ToolCalls is
// non-empty; if both are empty the agent loop treats the turn as a
// refusal.
type Response struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// FinishReason is a provider-specific hint ("stop", "tool_calls",
	// "length"). The agent loop stops on "stop" or empty.
	FinishReason string `json:"finish_reason,omitempty"`
}

// Provider is the LLM abstraction used by the agent loop. Implementations
// must be safe for concurrent use.
type Provider interface {
	// Chat sends a conversation to the model and returns its response.
	// tools is the list of tools the model may call; the provider is
	// free to ignore them (e.g. the stub). The context must cancel
	// in-flight HTTP requests.
	Chat(ctx context.Context, messages []Message, tools []ToolDescription) (*Response, error)
}

// New returns a Provider for the given config. Backend "stub" returns
// the StubProvider; "openai" returns an HTTP-backed OpenAIProvider;
// "vlm" returns a VLMProvider that delegates to internal/models/vlm
// (constructing a real OpenAI- or Anthropic-compatible client from the
// config). Unknown backends return a descriptive error.
func New(cfg config.ProviderConfig) (Provider, error) {
	switch cfg.Backend {
	case "", "stub":
		return NewStub(cfg), nil
	case "openai":
		return NewOpenAI(cfg), nil
	case "vlm":
		return NewVLMFromConfig(cfg)
	default:
		return nil, fmt.Errorf("providers: unknown backend %q (want one of stub, openai, vlm)", cfg.Backend)
	}
}

// ErrEmptyResponse is returned by providers when the model returns no
// content and no tool calls.
var ErrEmptyResponse = errors.New("providers: empty response")
