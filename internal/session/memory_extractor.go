package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// MemoryExtractor drives LLM-based extraction of facts / preferences /
// skills / events / relations from a session's turns. It is the entry
// point for the design-doc "记忆提取" pipeline; persistence of the
// resulting memory goes through Store.ApplyMemoryDiff.
type MemoryExtractor struct {
	llm LLMClient
}

// NewMemoryExtractor returns an extractor backed by the given LLM client.
// Passing nil is legal and yields a noop extractor that returns empty
// results; this keeps the Store usable before P12 wires the real client.
func NewMemoryExtractor(llm LLMClient) *MemoryExtractor {
	if llm == nil {
		llm = noopLLMClient{}
	}
	return &MemoryExtractor{llm: llm}
}

// Extract calls the LLM and returns the extracted memory items. Each item's
// ID and CreatedAt are populated if zero. SourceTurns are normalised to
// 0-based indices into sess.Turns.
func (m *MemoryExtractor) Extract(ctx context.Context, sess *domain.Session) ([]domain.ExtractedMemory, error) {
	if sess == nil {
		return nil, fmt.Errorf("memory_extractor: nil session")
	}
	if len(sess.Turns) == 0 {
		return nil, nil
	}
	mem, err := m.llm.Extract(ctx, sess.Turns)
	if err != nil {
		return nil, err
	}
	t := now()
	for i := range mem {
		if mem[i].ID == "" {
			mem[i].ID = newMemoryID()
		}
		if mem[i].CreatedAt.IsZero() {
			mem[i].CreatedAt = t
		}
		// Clamp confidence to [0,1].
		if mem[i].Confidence < 0 {
			mem[i].Confidence = 0
		}
		if mem[i].Confidence > 1 {
			mem[i].Confidence = 1
		}
	}
	return mem, nil
}

// Diff computes the MemoryDiff between the session's currently stored
// memory and a freshly extracted set. Items with IDs matching existing
// memory are treated as Updated; items with no matching ID are Added;
// existing IDs absent from the new set are Archived.
//
// The Diff is the basis for Store.ApplyMemoryDiff.
func (m *MemoryExtractor) Diff(existing []domain.ExtractedMemory, extracted []domain.ExtractedMemory) domain.MemoryDiff {
	byID := make(map[string]int, len(existing))
	for i, m := range existing {
		byID[m.ID] = i
	}
	newIDs := make(map[string]struct{}, len(extracted))
	diff := domain.MemoryDiff{}
	for _, m := range extracted {
		newIDs[m.ID] = struct{}{}
		if _, ok := byID[m.ID]; ok {
			diff.Updated = append(diff.Updated, m)
		} else {
			diff.Added = append(diff.Added, m)
		}
	}
	for _, m := range existing {
		if _, ok := newIDs[m.ID]; !ok {
			diff.Archived = append(diff.Archived, m.ID)
		}
	}
	return diff
}

// ErrEmptyLLMResult is returned when the LLM returns no memory and no
// error. Callers may treat this as a soft "nothing to persist" signal.
var ErrEmptyLLMResult = errors.New("memory_extractor: llm returned no memory")
