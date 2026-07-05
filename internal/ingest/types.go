// Package ingest implements the data-source ingestion pipeline that
// discovers agent conversation logs in supported harness formats, normalizes
// them into a unified message shape, and replays them into OpenViking
// session storage for memory extraction.
//
// The package mirrors openviking/ingest in the Python implementation. The
// major porting deltas are documented in docs/design/go-rewrite-design.md
// section 7.4: the registry is a plain Go map guarded by a sync.Mutex,
// the cursor store uses modernc.org/sqlite (pure-Go, no cgo), and the
// watch loop uses fsnotify + go-co-op/gocron/v2 instead of apscheduler.
package ingest

import "time"

// CursorKind enumerates the supported read-position pointer strategies.
const (
	// CursorByteOffset tracks an append-only log file by byte offset. The
	// cursor value is {"offset": int, "inode": uint64}.
	CursorByteOffset = "byte_offset"

	// CursorRowIDTime tracks a relational table by (time_created, id). The
	// cursor value is {"time": int64, "id": string}.
	CursorRowIDTime = "rowid_time"
)

// DefaultReadLimit is the maximum number of messages returned by a single
// Source.Read call. It bounds memory and lets the orchestrator append in
// idempotent batches of <=100 messages (the server-side batch cap).
const DefaultReadLimit = 100

// NormalizedMessage is a single conversation turn, harness-agnostic, ready
// for OpenViking replay. PeerID is resolved by the adapter (see peer.go):
// assistant turns map to "{harness}__{model}"; user turns map to a human
// identifier (git identity for single-user dev harnesses, original username
// for group-chat harnesses).
type NormalizedMessage struct {
	Role      string           `json:"role"`                 // "user" | "assistant"
	Text      string           `json:"text"`                 // primary text content
	Parts     []map[string]any `json:"parts,omitempty"`      // extra tool/context parts
	CreatedAt string           `json:"created_at,omitempty"` // ISO-8601
	PeerID    string           `json:"peer_id,omitempty"`
	Meta      map[string]any   `json:"meta,omitempty"` // model, provider, cwd, …
}

// SessionRef describes a discoverable conversation in a harness's storage.
type SessionRef struct {
	Harness         string         `json:"harness"`           // registry name
	NativeSessionID string         `json:"native_session_id"` // harness's own id
	Locator         string         `json:"locator"`           // file path or db session id
	Title           string         `json:"title,omitempty"`
	StartedAt       string         `json:"started_at,omitempty"` // ISO-8601
	Meta            map[string]any `json:"meta,omitempty"`       // session-level model, cwd, …
}

// Cursor is a durable read-position pointer for one (harness, session).
type Cursor struct {
	Kind  string         `json:"kind"`
	Value map[string]any `json:"value"`
}

// ZeroCursor returns the initial cursor for a kind.
func ZeroCursor(kind string) *Cursor {
	if kind == CursorRowIDTime {
		return &Cursor{Kind: kind, Value: map[string]any{"time": int64(0), "id": ""}}
	}
	return &Cursor{Kind: kind, Value: map[string]any{"offset": int64(0)}}
}

// Offset returns the byte-offset cursor value, or 0 when not set / wrong kind.
func (c *Cursor) Offset() int64 {
	if c == nil {
		return 0
	}
	v, _ := c.Value["offset"].(int64)
	return v
}

// Equal reports whether two cursors carry the same kind+value. nil and zero
// cursors of the same kind are equal.
func (c *Cursor) Equal(other *Cursor) bool {
	if c == nil {
		c = &Cursor{}
	}
	if other == nil {
		other = &Cursor{}
	}
	if c.Kind != other.Kind {
		return false
	}
	if len(c.Value) != len(other.Value) {
		return false
	}
	for k, v := range c.Value {
		ov, ok := other.Value[k]
		if !ok {
			return false
		}
		if !valueEqual(v, ov) {
			return false
		}
	}
	return true
}

// valueEqual compares two map[string]any values without recursing into nested
// maps (cursors are always flat).
func valueEqual(a, b any) bool {
	switch av := a.(type) {
	case int64:
		bv, ok := b.(int64)
		return ok && av == bv
	case int:
		switch bv := b.(type) {
		case int:
			return av == bv
		case int64:
			return int64(av) == bv
		}
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case uint64:
		switch bv := b.(type) {
		case uint64:
			return av == bv
		case int64:
			return int64(av) == bv
		}
	}
	return a == b
}

// ISOFromEpochMS returns a best-effort ISO-8601 timestamp from an
// epoch-millisecond int (used by SQLite/JSONL adapters). Returns "" when
// value cannot be parsed.
func ISOFromEpochMS(value any) string {
	if value == nil {
		return ""
	}
	var ms int64
	switch v := value.(type) {
	case int64:
		ms = v
	case int:
		ms = int64(v)
	case float64:
		ms = int64(v)
	case string:
		// fall through; let time.Parse handle ISO strings.
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			return ts.UTC().Format(time.RFC3339Nano)
		}
		return ""
	default:
		return ""
	}
	if ms <= 0 {
		return ""
	}
	// Heuristic: 10-digit values are seconds, 13-digit are milliseconds.
	var seconds float64
	if ms > 10_000_000_000 {
		seconds = float64(ms) / 1000.0
	} else {
		seconds = float64(ms)
	}
	t := time.Unix(int64(seconds), 0).UTC()
	if t.Year() < 1970 || t.Year() > 2100 {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}
