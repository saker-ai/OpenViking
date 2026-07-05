package ingest

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/session"
)

// sidAllowed matches characters disallowed in an OV session id. Anything
// matching is replaced with "-".
var sidAllowed = regexp.MustCompile(`[^a-zA-Z0-9_.@-]+`)

// sidCollapse collapses runs of "-" and trims leading/trailing "-.".
var sidCollapse = regexp.MustCompile(`-{2,}`)

// OVSessionID returns the OpenViking session id for (harness, nativeID).
// It is "{harness}__{nativeID}" with disallowed characters sanitized so
// the result always satisfies the OV identifier rules.
func OVSessionID(harness, nativeID string) string {
	clean := sidAllowed.ReplaceAllString(strings.TrimSpace(nativeID), "-")
	clean = sidCollapse.ReplaceAllString(clean, "-")
	clean = strings.Trim(clean, "-.")
	if clean == "" {
		clean = "unknown"
	}
	prefix := sidAllowed.ReplaceAllString(strings.TrimSpace(harness), "-")
	prefix = strings.Trim(prefix, "-.")
	if prefix == "" {
		prefix = "harness"
	}
	return prefix + PeerSep + clean
}

// Replayer replays NormalizedMessages into a session.Store. It is the
// Go equivalent of openviking/ingest/replay.SessionReplayer.
//
// The replay flow per session:
//  1. Reconcile  — look up the OV session; create it if missing.
//  2. AppendBatch — append up to DefaultReadLimit turns per call.
//  3. CommitIfNeeded — transition to committed status once idle.
//
// It is goroutine-safe only when backed by a goroutine-safe Store (the
// default MemoryStore is).
type Replayer struct {
	Store    *CursorStore
	Sessions session.Store
	Account  string
}

// NewReplayer wires a Replayer against the given cursor store and session
// store. account is the OV account under which replayed sessions are
// created.
func NewReplayer(store *CursorStore, sessions session.Store, account string) *Replayer {
	return &Replayer{Store: store, Sessions: sessions, Account: account}
}

// Reconcile ensures an OV session exists for (harness, nativeSessionID).
// It returns the existing session (or a freshly-created one) and a bool
// indicating whether it was newly created. The actual session ID assigned
// by the session.Store is recorded in the cursor store so subsequent runs
// can look it up.
func (r *Replayer) Reconcile(ctx context.Context, harness, nativeSessionID, locator, title string) (*domain.Session, bool, error) {
	// First, look up an existing cursor record to recover the
	// store-assigned session ID from a prior run.
	if r.Store != nil {
		rec, err := r.Store.Get(ctx, harness, nativeSessionID)
		if err != nil {
			return nil, false, err
		}
		if rec != nil && rec.OVSessionID != "" {
			existing, err := r.Sessions.Get(ctx, domain.Identifier{Account: r.Account}, rec.OVSessionID)
			if err == nil && existing != nil {
				return existing, false, nil
			}
			if err != nil && !isNotFound(err) {
				return nil, false, err
			}
			// Fall through to create a fresh session; the prior ID is
			// stale (session archived or never landed).
		}
	}
	created, err := r.Sessions.Create(ctx, domain.Identifier{Account: r.Account})
	if err != nil {
		return nil, false, err
	}
	if r.Store != nil {
		_ = r.Store.EnsureRow(ctx, harness, nativeSessionID, created.ID, ZeroCursor(CursorByteOffset), locator, title)
		_ = r.Store.AdvanceCursor(ctx, harness, nativeSessionID, created.ID, ZeroCursor(CursorByteOffset), locator)
	}
	return created, true, nil
}

// AppendBatch appends a slice of normalized messages as Turns on the OV
// session. Returns the number of turns actually appended (empty-text
// messages are dropped). The session is resolved via the cursor store
// record; call Reconcile first to ensure that record exists.
func (r *Replayer) AppendBatch(ctx context.Context, harness, nativeSessionID string, messages []NormalizedMessage) (int, error) {
	if len(messages) == 0 {
		return 0, nil
	}
	ovID, err := r.lookupSessionID(ctx, harness, nativeSessionID)
	if err != nil {
		return 0, err
	}
	id := domain.Identifier{Account: r.Account}
	appended := 0
	for _, msg := range messages {
		role, content, peerID, createdAt, ok := ToAddTurn(msg)
		if !ok {
			continue
		}
		turn := domain.Turn{
			Role:    domain.TurnRole(role),
			Content: content,
		}
		if peerID != "" {
			turn.ID = peerID
		}
		if createdAt != "" {
			if ts, err := parseISO(createdAt); err == nil {
				turn.CreatedAt = ts
			}
		}
		if err := r.Sessions.AppendTurn(ctx, id, ovID, turn); err != nil {
			return appended, err
		}
		appended++
	}
	if r.Store != nil && appended > 0 {
		_ = r.Store.IncrementAppended(ctx, harness, nativeSessionID, appended)
	}
	return appended, nil
}

// CommitIfNeeded transitions the OV session to committed status if it has
// pending appended-but-uncommitted turns. Returns true when a commit
// actually ran.
func (r *Replayer) CommitIfNeeded(ctx context.Context, harness, nativeSessionID string) (bool, error) {
	ovID, err := r.lookupSessionID(ctx, harness, nativeSessionID)
	if err != nil {
		return false, err
	}
	needsCommit := true
	if r.Store != nil {
		rec, err := r.Store.Get(ctx, harness, nativeSessionID)
		if err != nil {
			return false, err
		}
		if rec == nil {
			return false, nil
		}
		needsCommit = rec.NeedsCommit
	}
	if !needsCommit {
		return false, nil
	}
	id := domain.Identifier{Account: r.Account}
	if _, err := r.Sessions.Commit(ctx, id, ovID); err != nil {
		return false, err
	}
	if r.Store != nil {
		_ = r.Store.MarkCommitted(ctx, harness, nativeSessionID, 0)
	}
	return true, nil
}

// lookupSessionID resolves the store-assigned session ID for
// (harness, nativeSessionID) via the cursor store. Returns
// domain.ErrNotFound when no prior Reconcile has been called.
func (r *Replayer) lookupSessionID(ctx context.Context, harness, nativeSessionID string) (string, error) {
	if r.Store == nil {
		// Fall back to the deterministic OV alias; works only with
		// stores that honor caller-supplied IDs.
		return OVSessionID(harness, nativeSessionID), nil
	}
	rec, err := r.Store.Get(ctx, harness, nativeSessionID)
	if err != nil {
		return "", err
	}
	if rec == nil || rec.OVSessionID == "" {
		return "", domain.ErrNotFound
	}
	return rec.OVSessionID, nil
}

// ResetSession drops the cursor record for (harness, nativeSessionID) so
// the next backfill re-reads from the start. It does NOT delete the OV
// session; callers that want a full wipe should Archive the session
// separately.
func (r *Replayer) ResetSession(ctx context.Context, harness, nativeSessionID string) error {
	if r.Store == nil {
		return nil
	}
	// We do not expose a DeleteRow on the cursor store; an UPDATE to the
	// zero cursor achieves the same effect for re-reads.
	return r.Store.AdvanceCursor(ctx, harness, nativeSessionID,
		OVSessionID(harness, nativeSessionID),
		ZeroCursor(CursorByteOffset), "")
}

// parseISO parses an ISO-8601 timestamp string into time.Time. Accepts
// both RFC3339 and the looser "2006-01-02T15:04:05" layout used by some
// harness logs.
func parseISO(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("ingest: unparseable timestamp %q", s)
}

// isNotFound returns true when err carries the domain.ErrNotFound code.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var ae *domain.AppError
	if as := errors.As(err, &ae); as {
		return ae.Code == domain.CodeResourceNotFound
	}
	return false
}
