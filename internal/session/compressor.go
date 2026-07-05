package session

import (
	"context"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// Compressor reduces a session's turn history to fit within a configured
// token / turn budget. Implementations MUST be safe for concurrent use.
type Compressor interface {
	// Compress returns a compressed view of the session: a shortened turn
	// list, an optional Summary, and updated TokenUsage. The input is not
	// mutated.
	Compress(ctx context.Context, sess *domain.Session) (*domain.Session, error)
}

// CompressorConfig selects a compression strategy and its parameters. The
// zero value is not usable; construct via NewCompressor.
type CompressorConfig struct {
	// Version: "v2", "v3", or "auto" (default "v2").
	Version string
	// LLM is used by v3 for summary generation. Required for v3; ignored
	// by v2. If nil and Version=="v3", the factory substitutes a noop.
	LLM LLMClient
	// MaxContextTokens triggers compression when a session exceeds this
	// token budget. Default 32000.
	MaxContextTokens int
	// MaxTurns triggers compression when a session exceeds this many
	// turns. Default 20.
	MaxTurns int
	// KeepLastNTurns is the number of most-recent turns preserved by v2
	// truncation. Default 10.
	KeepLastNTurns int
	// AutoV3Turns is the turn count above which "auto" picks v3. Default 50.
	AutoV3Turns int
	// SummaryChunkSize is the max turns per LLM summarize call in v3.
	// Default 20.
	SummaryChunkSize int
}

// withDefaults returns a copy of cfg with zero fields populated.
func (cfg CompressorConfig) withDefaults() CompressorConfig {
	out := cfg
	if out.Version == "" {
		out.Version = "v2"
	}
	if out.MaxContextTokens <= 0 {
		out.MaxContextTokens = 32000
	}
	if out.MaxTurns <= 0 {
		out.MaxTurns = 20
	}
	if out.KeepLastNTurns <= 0 {
		out.KeepLastNTurns = 10
	}
	if out.AutoV3Turns <= 0 {
		out.AutoV3Turns = 50
	}
	if out.SummaryChunkSize <= 0 {
		out.SummaryChunkSize = 20
	}
	if out.LLM == nil {
		out.LLM = noopLLMClient{}
	}
	return out
}

// shouldCompress reports whether the session crosses either configured
// threshold. Both compressors use it as the trigger gate.
func shouldCompress(sess *domain.Session, cfg CompressorConfig) bool {
	if sess == nil {
		return false
	}
	if len(sess.Turns) > cfg.MaxTurns {
		return true
	}
	total := 0
	for _, t := range sess.Turns {
		total += t.Tokens
	}
	return total > cfg.MaxContextTokens
}

// countTokens sums the per-turn token counts.
func countTokens(turns []domain.Turn) int {
	total := 0
	for _, t := range turns {
		total += t.Tokens
	}
	return total
}
