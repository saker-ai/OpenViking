// Package vlm — in-process stub for tests and bootstrap.
//
// The stub returns canned responses without any network or process side
// effects. Use it in unit tests for retrieve, session, and ingest pipelines
// to avoid coupling test fixtures to a live model. It is also the default
// VLM returned by NewStub when no provider is configured, so the server can
// boot end-to-end before P12 wires real credentials.
package vlm

import (
	"context"
	"sync"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// Stub is a VLM that returns scripted responses. Set ChatFn / EmbedFn to
// inject custom behavior; otherwise the zero-value stub returns
// domain.ErrUnsupported so tests fail loudly if they forget to script a
// response.
type Stub struct {
	mu      sync.Mutex
	ChatFn  func(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	EmbedFn func(ctx context.Context, texts []string) ([][]float32, error)

	// Calls records every Chat and Embed invocation in arrival order.
	// Tests may inspect it to assert request shapes.
	Calls []StubCall
}

// StubCall is one recorded interaction with the stub.
type StubCall struct {
	Kind  string // "chat" | "embed"
	Chat  *ChatRequest
	Texts []string
}

// Chat implements VLM. If ChatFn is nil, returns ErrUnsupported.
func (s *Stub) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	s.mu.Lock()
	s.Calls = append(s.Calls, StubCall{Kind: "chat", Chat: &req})
	s.mu.Unlock()
	if s.ChatFn == nil {
		return nil, domain.ErrUnsupported
	}
	return s.ChatFn(ctx, req)
}

// Embed implements VLM. If EmbedFn is nil, returns ErrUnsupported.
func (s *Stub) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	s.Calls = append(s.Calls, StubCall{Kind: "embed", Texts: append([]string(nil), texts...)})
	s.mu.Unlock()
	if s.EmbedFn == nil {
		return nil, domain.ErrUnsupported
	}
	return s.EmbedFn(ctx, texts)
}

// Reset clears all recorded calls. Safe for concurrent use.
func (s *Stub) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = nil
}

// NewStub returns an empty Stub. Equivalent to &Stub{}; provided for
// symmetry with NewOpenAI / NewAnthropic.
func NewStub() *Stub { return &Stub{} }

// Compile-time assertion that Stub satisfies VLM.
var _ VLM = (*Stub)(nil)
