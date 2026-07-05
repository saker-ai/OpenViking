// Package vlm defines the vision-language model (VLM) adapter layer used by
// the OpenViking retrieve, session, and ingest pipelines.
//
// The VLM interface is provider-agnostic: every implementation (OpenAI,
// Volcengine Ark, Anthropic, LiteLLM, stub) accepts the same ChatRequest and
// returns the same ChatResponse. Concrete clients are constructed via the
// New* functions in their respective files; tests inject a Stub to avoid
// network calls.
//
// All external HTTP is performed through an *http.Client (overridable via
// the constructor) so tests can substitute an httptest.Server recorder
// without pulling in provider SDKs. Tests MUST NOT make network calls.
package vlm

import (
	"context"
	"net/http"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// Role enumerates the role of a single chat message. Aligns with OpenAI's
// "system|user|assistant|tool" enum; Anthropic maps "system" into a top-level
// system parameter (see anthropic.go).
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a single chat message. Content is plain text; multimodal
// payloads (images, audio) are intentionally out of scope for the first cut
// and will be added as a Part []ContentPart field when needed.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is a single tool invocation requested by the assistant.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string, decoded by caller
}

// Tool describes a callable tool exposed to the model. Schema is a JSON Schema
// object (a map[string]any) describing the arguments the tool accepts.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
}

// ResponseFormat constrains the model output. "text" (default) leaves the
// output free-form; "json_object" requests JSON; "json_schema" pins a
// specific schema via JSONSchema.
type ResponseFormat struct {
	Type       string         `json:"type"` // "text" | "json_object" | "json_schema"
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

// ChatRequest is the provider-agnostic input to VLM.Chat.
type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Tools          []Tool          `json:"tools,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	Temperature    float64         `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	TopP           float64         `json:"top_p,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	// Stream is currently ignored; streaming support lands in a later phase.
	Stream bool `json:"stream,omitempty"`
}

// ChatResponse is the provider-agnostic output of VLM.Chat.
type ChatResponse struct {
	Model        string     `json:"model"`
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason"`
	Usage        Usage      `json:"usage"`
}

// Usage records token consumption for a single chat call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// VLM is the provider-agnostic vision-language model interface implemented
// by every adapter. Implementations must be safe for concurrent use and
// must respect ctx cancellation.
//
// Chat performs a single non-streaming chat completion.
//
// Embed is a convenience method that calls the underlying embedding endpoint
// exposed by the same provider. For providers that do not ship an embedding
// endpoint (e.g. Anthropic), Embed returns domain.ErrUnsupported and callers
// should route embedding traffic through a dedicated embedder.Embedder
// instead.
type VLM interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Doer is the minimal HTTP round-tripper interface used by every VLM client.
// *http.Client satisfies it; tests inject a recording transport backed by
// httptest.Server. Defining it here avoids importing provider SDKs and keeps
// the package testable without network access.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// wrapVLM annotates an error with the VLM business code.
func wrapVLM(err error) error {
	if err == nil {
		return nil
	}
	return domain.Wrap(domain.CodeVLMFailed, 502, err)
}

// ErrUnsupported is re-exported so adapters in this package can return a
// stable sentinel without importing domain in their constructors. It is
// the same value as domain.ErrUnsupported.
var ErrUnsupported = domain.ErrUnsupported
