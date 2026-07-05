package session

import (
	"context"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// LLMClient abstracts the LLM calls used by compressors and the memory
// extractor. The real implementation is wired in P12 against
// internal/models/vlm; unit tests inject a fake.
//
// Both methods are context-aware so callers can cancel in-flight VLM
// requests. They must be safe to call concurrently.
type LLMClient interface {
	// Summarize condenses a sequence of turns into a short natural-language
	// summary suitable for prepending to a compressed session. The returned
	// string MUST be plain text (no JSON envelope).
	Summarize(ctx context.Context, turns []domain.Turn) (string, error)

	// Extract inspects a sequence of turns and returns the facts /
	// preferences / skills / events / relations worth persisting as
	// long-term memory. Each item's SourceTurns should reference the
	// 0-based indices of the turns it was derived from.
	Extract(ctx context.Context, turns []domain.Turn) ([]domain.ExtractedMemory, error)
}

// noopLLMClient is the default LLM client used when none is wired. It returns
// empty results without error so the rest of the pipeline can run in tests
// and during early bootstrap before P12 lands.
type noopLLMClient struct{}

func (noopLLMClient) Summarize(ctx context.Context, turns []domain.Turn) (string, error) {
	return "", nil
}

func (noopLLMClient) Extract(ctx context.Context, turns []domain.Turn) ([]domain.ExtractedMemory, error) {
	return nil, nil
}
