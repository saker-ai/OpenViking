package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// CompressorV2 implements the naive-truncation strategy. It preserves the
// first system / user turn (context anchor) and the last N turns, dropping
// the middle. A short Summary is synthesized from the dropped turns without
// calling the LLM so v2 is usable with a nil LLMClient.
//
// The compressed Session is a copy; the input is not mutated.
type CompressorV2 struct {
	cfg CompressorConfig
}

// NewCompressorV2 returns a v2 compressor with the given configuration.
func NewCompressorV2(cfg CompressorConfig) *CompressorV2 {
	return &CompressorV2{cfg: cfg.withDefaults()}
}

// Compress implements Compressor. If the session is below both thresholds it
// is returned unchanged (apart from cloning).
func (c *CompressorV2) Compress(ctx context.Context, sess *domain.Session) (*domain.Session, error) {
	if sess == nil {
		return nil, fmt.Errorf("compressor_v2: nil session")
	}
	out := cloneSession(sess)
	if !shouldCompress(out, c.cfg) {
		return out, nil
	}
	turns := out.Turns
	if len(turns) == 0 {
		return out, nil
	}
	keep := c.cfg.KeepLastNTurns
	if keep <= 0 {
		keep = 10
	}
	// Ensure we drop at least one turn; otherwise compression is a no-op.
	if keep >= len(turns) {
		keep = len(turns) - 1
		if keep <= 0 {
			return out, nil
		}
	}
	anchor := anchorIndex(turns)
	tailStart := len(turns) - keep
	if anchor >= tailStart {
		// Anchor is already inside the tail; keep only the last `keep`.
		dropped := turns[:tailStart]
		out.Summary = synthesizeSummary(dropped)
		out.Turns = turns[tailStart:]
	} else {
		head := turns[anchor]
		dropped := turns[anchor+1 : tailStart]
		tail := turns[tailStart:]
		if len(dropped) == 0 {
			// No middle to drop; drop the head so compression actually
			// reduces the turn count.
			out.Summary = synthesizeSummary([]domain.Turn{head})
			out.Turns = tail
		} else {
			out.Summary = synthesizeSummary(dropped)
			out.Turns = append([]domain.Turn{head}, tail...)
		}
	}
	out.TokenUsage = domain.TokenUsage{
		PromptTokens:     countTokens(out.Turns),
		CompletionTokens: 0,
	}
	out.TokenUsage.TotalTokens = out.TokenUsage.PromptTokens + out.TokenUsage.CompletionTokens
	return out, nil
}

// anchorIndex returns the index of the first turn to preserve as the
// context anchor (the leading system or user message). If none exists it
// returns 0.
func anchorIndex(turns []domain.Turn) int {
	for i, t := range turns {
		if t.Role == domain.TurnRoleUser || t.Role == domain.TurnRoleAssistant {
			return i
		}
	}
	return 0
}

// synthesizeSummary builds a plain-text summary of the dropped turns
// without invoking the LLM. It records the role and a truncated content
// snippet of each dropped turn.
func synthesizeSummary(dropped []domain.Turn) string {
	if len(dropped) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[v2 truncation] dropped %d turn(s):\n", len(dropped)))
	for _, t := range dropped {
		snippet := t.Content
		if len(snippet) > 80 {
			snippet = snippet[:80] + "..."
		}
		b.WriteString(fmt.Sprintf("- %s: %s\n", t.Role, snippet))
	}
	return b.String()
}
