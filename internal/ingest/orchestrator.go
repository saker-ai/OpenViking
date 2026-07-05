package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// BackfillStats accumulates per-source backfill counters.
type BackfillStats struct {
	Sessions  int      `json:"sessions"`
	Messages  int      `json:"messages"`
	Committed int      `json:"committed"`
	Skipped   int      `json:"skipped"`
	Errors    []string `json:"errors,omitempty"`
}

// Merge adds other's counters into s. Errors are appended.
func (s *BackfillStats) Merge(other BackfillStats) {
	s.Sessions += other.Sessions
	s.Messages += other.Messages
	s.Committed += other.Committed
	s.Skipped += other.Skipped
	s.Errors = append(s.Errors, other.Errors...)
}

// Orchestrator coordinates backfill across multiple registered sources.
// It mirrors openviking/ingest/orchestrator.IngestOrchestrator: for each
// enabled source, discover sessions, replay cursor->end, advance the
// cursor, and commit when idle.
type Orchestrator struct {
	Replayer *Replayer
	Sources  map[string]Source
	mu       sync.Mutex
}

// NewOrchestrator constructs an orchestrator over the given sources.
// replayer may be nil; callers that already have a Replayer should pass
// it directly.
func NewOrchestrator(replayer *Replayer, sources map[string]Source) *Orchestrator {
	if sources == nil {
		sources = map[string]Source{}
	}
	return &Orchestrator{Replayer: replayer, Sources: sources}
}

// Register attaches a source under name. Re-registering overwrites.
func (o *Orchestrator) Register(name string, src Source) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Sources[name] = src
}

// BackfillSource runs a single source's backfill: discover sessions, read
// cursor->end in DefaultReadLimit batches, append+commit each batch.
func (o *Orchestrator) BackfillSource(ctx context.Context, name string, src Source, opts BackfillOptions) BackfillStats {
	stats := BackfillStats{}
	if err := src.Scan(ctx, func(ref SessionRef) error {
		if opts.Since != "" && ref.StartedAt != "" && ref.StartedAt < opts.Since {
			stats.Skipped++
			return nil
		}
		added, err := o.backfillOne(ctx, name, src, ref, opts)
		if err != nil {
			if errors.Is(err, domain.ErrUnsupported) {
				// Source is registered but unusable in current config
				// (e.g. legacy file-store). Log and move on.
				stats.Errors = append(stats.Errors,
					fmt.Sprintf("%s: %s", name, err.Error()))
				return err
			}
			stats.Errors = append(stats.Errors,
				fmt.Sprintf("%s/%s: %s", name, ref.NativeSessionID, err.Error()))
			return nil
		}
		stats.Sessions++
		stats.Messages += added
		if !opts.DryRun {
			if committed, err := o.commitIfNeeded(ctx, name, ref, opts); err != nil {
				stats.Errors = append(stats.Errors,
					fmt.Sprintf("%s/%s: commit: %s", name, ref.NativeSessionID, err.Error()))
			} else if committed {
				stats.Committed++
			}
		}
		return nil
	}); err != nil {
		stats.Errors = append(stats.Errors, fmt.Sprintf("%s: scan: %s", name, err.Error()))
		return stats
	}
	return stats
}

// BackfillOptions configures a single backfill pass.
type BackfillOptions struct {
	// Since filters out sessions whose StartedAt is older than this RFC3339
	// timestamp. Empty disables the filter.
	Since string
	// DryRun skips replay/commit; only counts messages that would be
	// appended.
	DryRun bool
	// Reset drops the cursor record for each session before backfill so
	// the source is re-read from the start.
	Reset bool
}

// Backfill runs BackfillSource for every registered source. Returns a
// map of source-name -> stats.
func (o *Orchestrator) Backfill(ctx context.Context, opts BackfillOptions) map[string]BackfillStats {
	o.mu.Lock()
	names := make([]string, 0, len(o.Sources))
	for n := range o.Sources {
		names = append(names, n)
	}
	o.mu.Unlock()

	results := map[string]BackfillStats{}
	for _, name := range names {
		o.mu.Lock()
		src := o.Sources[name]
		o.mu.Unlock()
		if src == nil {
			continue
		}
		stats := o.BackfillSource(ctx, name, src, opts)
		results[name] = stats
	}
	return results
}

// backfillOne reads cursor->end for one session, appending batches.
func (o *Orchestrator) backfillOne(ctx context.Context, name string, src Source, ref SessionRef, opts BackfillOptions) (int, error) {
	if o.Replayer == nil {
		return 0, domain.NewAppError(domain.CodeInternalError, 500,
			"ingest: orchestrator has no replayer")
	}
	if opts.Reset {
		if err := o.Replayer.ResetSession(ctx, name, ref.NativeSessionID); err != nil {
			return 0, err
		}
	}
	if !opts.DryRun {
		if _, _, err := o.Replayer.Reconcile(ctx, name, ref.NativeSessionID, ref.Locator, ref.Title); err != nil {
			return 0, err
		}
	}
	total := 0
	kind := src.CursorKind()
	cursor := o.loadCursor(ctx, name, ref.NativeSessionID, kind)
	for {
		messages, newCursor, err := src.Read(ctx, ref, cursor, DefaultReadLimit)
		if err != nil {
			return total, err
		}
		if len(messages) == 0 {
			// EOF; persist the advanced cursor (file size grew).
			if !opts.DryRun && newCursor != nil && !newCursor.Equal(cursor) {
				ovID, _ := o.Replayer.lookupSessionID(ctx, name, ref.NativeSessionID)
				if ovID == "" {
					ovID = OVSessionID(name, ref.NativeSessionID)
				}
				_ = o.Replayer.Store.AdvanceCursor(ctx, name, ref.NativeSessionID, ovID, newCursor, ref.Locator)
			}
			return total, nil
		}
		if opts.DryRun {
			for _, m := range messages {
				if strings.TrimSpace(m.Text) != "" || len(m.Parts) > 0 {
					total++
				}
			}
		} else {
			added, err := o.Replayer.AppendBatch(ctx, name, ref.NativeSessionID, messages)
			if err != nil {
				return total, err
			}
			total += added
		}
		if !opts.DryRun && newCursor != nil {
			ovID, _ := o.Replayer.lookupSessionID(ctx, name, ref.NativeSessionID)
			if ovID == "" {
				ovID = OVSessionID(name, ref.NativeSessionID)
			}
			if o.Replayer.Store != nil {
				_ = o.Replayer.Store.SetPending(ctx, name, ref.NativeSessionID, newCursor, len(messages), 0)
				_ = o.Replayer.Store.AdvanceCursor(ctx, name, ref.NativeSessionID, ovID, newCursor, ref.Locator)
				_ = o.Replayer.Store.ClearPending(ctx, name, ref.NativeSessionID)
			}
		}
		cursor = newCursor
	}
}

// commitIfNeeded wraps Replayer.CommitIfNeeded with stats-aware error
// handling. A no-op commit (no pending turns) is not an error.
func (o *Orchestrator) commitIfNeeded(ctx context.Context, name string, ref SessionRef, opts BackfillOptions) (bool, error) {
	if o.Replayer == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return o.Replayer.CommitIfNeeded(ctx, name, ref.NativeSessionID)
}

// loadCursor returns the persisted cursor for (name, nativeID) or a fresh
// zero cursor of the right kind.
func (o *Orchestrator) loadCursor(ctx context.Context, name, nativeID, kind string) *Cursor {
	if o.Replayer == nil || o.Replayer.Store == nil {
		return ZeroCursor(kind)
	}
	cur, err := o.Replayer.Store.GetCursor(ctx, name, nativeID, kind)
	if err != nil || cur == nil {
		return ZeroCursor(kind)
	}
	return cur
}
