package session

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// CompressorV3 implements the summary-based strategy. It slices the turn
// history into chunks, asks the LLMClient to summarize each chunk, prepends
// a single "summary" assistant turn to the kept tail, and drops the rest.
//
// On any LLM failure, v3 falls back to v2 truncation (per design doc:
// "失败回退: 自动降级为 v2").
type CompressorV3 struct {
	cfg CompressorConfig
	v2  *CompressorV2
}

// NewCompressorV3 returns a v3 compressor. cfg.LLM must be non-nil for
// meaningful summaries; a noop LLM produces empty summaries but the
// pipeline still runs.
func NewCompressorV3(cfg CompressorConfig) *CompressorV3 {
	cfg = cfg.withDefaults()
	return &CompressorV3{
		cfg: cfg,
		v2:  NewCompressorV2(cfg),
	}
}

// Compress implements Compressor.
func (c *CompressorV3) Compress(ctx context.Context, sess *domain.Session) (*domain.Session, error) {
	if sess == nil {
		return nil, fmt.Errorf("compressor_v3: nil session")
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
	if len(turns) <= keep {
		return out, nil
	}
	dropped := turns[:len(turns)-keep]
	tail := turns[len(turns)-keep:]

	summary, err := c.summarizeDropped(ctx, dropped)
	if err != nil {
		// Fallback: degrade to v2 truncation semantics.
		return c.v2.Compress(ctx, sess)
	}

	anchor := anchorIndex(turns)
	var head []domain.Turn
	if anchor < len(dropped) {
		head = []domain.Turn{turns[anchor]}
	}

	summaryTurn := domain.Turn{
		ID:        newTurnID(),
		Role:      domain.TurnRoleAssistant,
		Content:   "[v3 summary] " + summary,
		Tokens:    countTokens(dropped) / 4, // crude estimate
		CreatedAt: now(),
	}
	out.Summary = summary
	out.Turns = append(head, summaryTurn)
	out.Turns = append(out.Turns, tail...)
	out.TokenUsage = domain.TokenUsage{
		PromptTokens:     countTokens(out.Turns),
		CompletionTokens: 0,
	}
	out.TokenUsage.TotalTokens = out.TokenUsage.PromptTokens + out.TokenUsage.CompletionTokens
	return out, nil
}

// summarizeDropped splits the dropped turns into chunks and summarizes each
// concurrently. Chunks are processed in order so the partial summaries can
// be concatenated deterministically.
func (c *CompressorV3) summarizeDropped(ctx context.Context, dropped []domain.Turn) (string, error) {
	if len(dropped) == 0 {
		return "", nil
	}
	chunk := c.cfg.SummaryChunkSize
	if chunk <= 0 {
		chunk = 20
	}
	chunks := chunkTurns(dropped, chunk)
	if len(chunks) == 0 {
		return "", nil
	}
	summaries := make([]string, len(chunks))
	errs := make([]error, len(chunks))
	var wg sync.WaitGroup
	for i, ch := range chunks {
		wg.Add(1)
		go func(idx int, in []domain.Turn) {
			defer wg.Done()
			s, err := c.cfg.LLM.Summarize(ctx, in)
			summaries[idx] = s
			errs[idx] = err
		}(i, ch)
	}
	wg.Wait()
	// First error wins; caller falls back to v2.
	for _, err := range errs {
		if err != nil {
			return "", err
		}
	}
	parts := make([]string, 0, len(summaries))
	for _, s := range summaries {
		s = strings.TrimSpace(s)
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// chunkTurns splits turns into slices of at most n turns.
func chunkTurns(turns []domain.Turn, n int) [][]domain.Turn {
	if n <= 0 || len(turns) == 0 {
		return nil
	}
	var out [][]domain.Turn
	for i := 0; i < len(turns); i += n {
		end := i + n
		if end > len(turns) {
			end = len(turns)
		}
		out = append(out, turns[i:end])
	}
	return out
}
