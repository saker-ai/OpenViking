package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/saker-ai/ctxhub/internal/ragfs"
)

// FeedbackStats is a single row in the feedback-stats output table.
// Period is a human label like "7d" or "24h"; Positive/Negative are
// counts of thumbs-up/down feedback received within the period.
// NetScore is Positive - Negative.
type FeedbackStats struct {
	Period   string `json:"period"`
	Positive int    `json:"positive"`
	Negative int    `json:"negative"`
	NetScore int    `json:"net_score"`
}

// ErrNoFeedbackData is returned by FeedbackStore.Stats when no feedback
// data is available. The CLI translates this into a friendly "no
// feedback data" message and exit 0.
var ErrNoFeedbackData = errors.New("cli: no feedback data")

// FeedbackStore aggregates user feedback (thumbs up/down). The default
// implementation is a stub that returns ErrNoFeedbackData; production
// implementations read from the bot's session/feedback persistence.
type FeedbackStore interface {
	// Stats returns feedback aggregated over the given lookback window.
	// since <= 0 means "all time". Returns ErrNoFeedbackData when no
	// feedback has been recorded.
	Stats(ctx context.Context, since time.Duration) ([]FeedbackStats, error)
}

// StubFeedbackStore always returns ErrNoFeedbackData. It is the
// default FeedbackStore for the CLI until the bot's feedback
// persistence layer is wired (tracked separately).
type StubFeedbackStore struct{}

// NewStubFeedbackStore returns a StubFeedbackStore.
func NewStubFeedbackStore() *StubFeedbackStore { return &StubFeedbackStore{} }

// Stats returns ErrNoFeedbackData.
func (s *StubFeedbackStore) Stats(_ context.Context, _ time.Duration) ([]FeedbackStats, error) {
	return nil, fmt.Errorf("%w", ErrNoFeedbackData)
}

// feedbackEntry is the on-disk JSON shape of a single feedback record stored
// under /accounts/{account}/feedback/{id}.json.
type feedbackEntry struct {
	Rating    string    `json:"rating"`
	CreatedAt time.Time `json:"created_at"`
}

// RagfsFeedbackStore reads feedback records from ragfs. Each file under
// /accounts/{account}/feedback/ is a JSON feedbackEntry with rating
// ("up" or "down") and created_at.
type RagfsFeedbackStore struct {
	fs      ragfs.FileSystem
	account string
}

// NewRagfsFeedbackStore returns a RagfsFeedbackStore rooted at the given
// account's feedback directory.
func NewRagfsFeedbackStore(fs ragfs.FileSystem, account string) *RagfsFeedbackStore {
	return &RagfsFeedbackStore{fs: fs, account: account}
}

// Stats aggregates feedback records into period buckets. When since <= 0
// only the "all" row is returned. Empty or missing directories yield
// ErrNoFeedbackData.
func (s *RagfsFeedbackStore) Stats(ctx context.Context, since time.Duration) ([]FeedbackStats, error) {
	dir := ragfs.Normalize(path.Join("/accounts", s.account, "feedback"))
	entries, err := s.fs.ReadDir(ctx, dir)
	if err != nil {
		if ragfs.IsNotFound(err) {
			return nil, fmt.Errorf("%w", ErrNoFeedbackData)
		}
		return nil, err
	}
	buckets := newFeedbackBuckets(since)
	cutoff := time.Now().Add(-since)
	any := false
	for _, e := range entries {
		if e == nil || e.Info == nil || e.Info.IsDir {
			continue
		}
		name := e.Info.Name
		if len(name) <= 5 || name[len(name)-5:] != ".json" {
			continue
		}
		fb, ferr := s.readEntry(ctx, ragfs.Normalize(path.Join(dir, name)))
		if ferr != nil || fb == nil {
			continue
		}
		any = true
		if since > 0 && fb.CreatedAt.Before(cutoff) {
			continue
		}
		buckets.add(fb)
	}
	if !any {
		return nil, fmt.Errorf("%w", ErrNoFeedbackData)
	}
	return buckets.buildRows(), nil
}

func (s *RagfsFeedbackStore) readEntry(ctx context.Context, p string) (*feedbackEntry, error) {
	var buf bytes.Buffer
	if err := s.fs.Read(ctx, p, &buf); err != nil {
		return nil, err
	}
	var fb feedbackEntry
	if err := json.Unmarshal(buf.Bytes(), &fb); err != nil {
		return nil, err
	}
	return &fb, nil
}

// feedbackBuckets accumulates up/down counts per period label.
type feedbackBuckets struct {
	rows  []FeedbackStats
	since time.Duration
}

func newFeedbackBuckets(since time.Duration) *feedbackBuckets {
	if since <= 0 {
		return &feedbackBuckets{rows: []FeedbackStats{{Period: "all"}}}
	}
	return &feedbackBuckets{
		rows: []FeedbackStats{
			{Period: "24h"},
			{Period: "7d"},
			{Period: "30d"},
			{Period: "all"},
		},
		since: since,
	}
}

func (b *feedbackBuckets) add(fb *feedbackEntry) {
	idx := -1
	switch {
	case b.since <= 0:
		idx = 0
	case time.Since(fb.CreatedAt) < 24*time.Hour:
		idx = 0
	case time.Since(fb.CreatedAt) < 7*24*time.Hour:
		idx = 1
	case time.Since(fb.CreatedAt) < 30*24*time.Hour:
		idx = 2
	default:
		idx = 3
	}
	if idx < 0 || idx >= len(b.rows) {
		return
	}
	if fb.Rating == "up" {
		b.rows[idx].Positive++
	} else if fb.Rating == "down" {
		b.rows[idx].Negative++
	}
}

func (b *feedbackBuckets) buildRows() []FeedbackStats {
	out := make([]FeedbackStats, len(b.rows))
	for i, r := range b.rows {
		r.NetScore = r.Positive - r.Negative
		out[i] = r
	}
	return out
}
