package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ingest"
)

// CursorIDESource parses Cursor IDE chat history from state.vscdb.
//
// NOTE: this is the Cursor *IDE harness*, distinct from the read-position
// Cursor pointer in ingest.Cursor.
//
// Cursor stores chat in
// ~/Library/Application Support/Cursor/User/{globalStorage,workspaceStorage/*}/state.vscdb
// (SQLite key-value: ItemTable / cursorDiskKV), with a single conversation
// spread across composerData:* / bubbleId:* / agentKv:* BLOB→JSON keys.
// The schema is undocumented and drifts between Cursor versions.
//
// This adapter does a best-effort scan: it queries ItemTable for
// composerData:* keys (one per composer conversation), decodes the JSON
// blob to extract a composer_id + title, and emits one SessionRef per
// composer. RowsToMessages then walks cursorDiskKV bubbleId:* entries
// belonging to that composer and decodes each bubble's text.
//
// When the schema does not match (no ItemTable, no composerData keys,
// or JSON blobs that don't carry the expected fields), the adapter
// returns a clear, actionable error rather than a generic unsupported
// sentinel — so callers can file a bug or upgrade Cursor.
type CursorIDESource struct {
	SqliteLogSource
}

func init() {
	ingest.Register("cursor", func(cfg ingest.SourceConfig) (ingest.Source, error) {
		src := NewCursorIDESource()
		if len(cfg.Paths) > 0 {
			src.Paths = cfg.Paths
		}
		if cfg.User != "" {
			src.FallbackUser = cfg.User
		}
		return src, nil
	})
}

// NewCursorIDESource constructs a CursorIDESource with default roots.
func NewCursorIDESource() *CursorIDESource {
	src := &CursorIDESource{
		SqliteLogSource: SqliteLogSource{
			Name:         "cursor",
			Paths:        []string{"~/Library/Application Support/Cursor/User/globalStorage/state.vscdb"},
			FallbackUser: "default",
		},
	}
	src.dbPath = src.DBPath
	src.discover = src.Discover
	src.fetchRows = src.FetchRows
	src.rowComplete = src.RowComplete
	src.rowsToMessages = src.RowsToMessages
	return src
}

// DBPath returns the configured SQLite database path.
func (s *CursorIDESource) DBPath() string {
	roots := s.roots()
	if len(roots) > 0 {
		return expandTilde(roots[0])
	}
	return expandTilde("~/Library/Application Support/Cursor/User/globalStorage/state.vscdb")
}

// cursorComposerKey prefixes the JSON keys that store composer metadata
// in Cursor's ItemTable. Cursor versions that ship composer chat use
// `composerData:<composer_id>` as the key.
const cursorComposerKey = "composerData:"

// cursorBubbleTable is the SQLite table that stores conversation bubbles
// in newer Cursor versions. Older versions stored them in ItemTable with
// `bubbleId:<composer_id>:<bubble_id>` keys.
const cursorBubbleTable = "cursorDiskKV"

// Discover enumerates composer conversations from the SQLite DB by
// scanning ItemTable for `composerData:*` keys.
func (s *CursorIDESource) Discover(ctx context.Context, conn *sql.DB, fn func(ingest.SessionRef) error) error {
	hasTable, err := tableExists(ctx, conn, "ItemTable")
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("cursor: check ItemTable: %w", err))
	}
	if !hasTable {
		return domain.NewAppError(domain.CodeUnsupported, 501,
			"cursor: state.vscdb has no ItemTable (Cursor schema unrecognized); "+
				"file a bug with the Cursor version and the table list")
	}

	rows, err := conn.QueryContext(ctx,
		"SELECT key, value FROM ItemTable WHERE key LIKE ? ORDER BY key",
		cursorComposerKey+"%")
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("cursor: query composerData: %w", err))
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var key, value sql.NullString
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}
		if !key.Valid || !value.Valid {
			continue
		}
		composerID := strings.TrimPrefix(key.String, cursorComposerKey)
		if composerID == "" {
			continue
		}
		title, startedAt := parseCursorComposerMeta(value.String)
		ref := ingest.SessionRef{
			Harness:         s.Name,
			NativeSessionID: composerID,
			Locator:         composerID,
			Title:           title,
			StartedAt:       startedAt,
			Meta: map[string]any{
				"composer_id": composerID,
				"source":      "state.vscdb",
			},
		}
		if err := fn(ref); err != nil {
			return err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	if count == 0 {
		// Real error: the DB exists and has ItemTable but no composer
		// conversations. Surface it so the caller knows Cursor has no
		// chat history to ingest (or the schema changed).
		return domain.NewAppError(domain.CodeUnsupported, 501,
			"cursor: no composerData:* keys in ItemTable; "+
				"either Cursor has no chat history or the schema changed")
	}
	return nil
}

// FetchRows returns up to limit bubble rows for the composer identified
// by ref.Locator, after the (time_created, id) cursor. Looks first in
// the cursorDiskKV table (newer Cursor) and falls back to ItemTable
// `bubbleId:<composer>:*` keys (older Cursor). Returns a clear error
// when neither schema is present.
func (s *CursorIDESource) FetchRows(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, cursor *ingest.Cursor, limit int) (*sql.Rows, error) {
	if limit <= 0 {
		limit = ingest.DefaultReadLimit
	}
	var t int64
	var lastID string
	if cursor != nil {
		t = toInt64(cursor.Value["time"])
		if id, ok := cursor.Value["id"].(string); ok {
			lastID = id
		}
	}
	composerID := ref.Locator
	if composerID == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"cursor: ref.Locator (composer_id) is empty")
	}

	hasDiskKV, err := tableExists(ctx, conn, cursorBubbleTable)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("cursor: check %s: %w", cursorBubbleTable, err))
	}
	if hasDiskKV {
		// cursorDiskKV schema: (key TEXT PRIMARY KEY, value BLOB, creation_date INTEGER)
		// Keys for bubbles: "bubbleId:<composer_id>:<bubble_id>".
		prefix := "bubbleId:" + composerID + ":"
		return conn.QueryContext(ctx, `
			SELECT key, value, creation_date FROM `+cursorBubbleTable+`
			WHERE key LIKE ?
			  AND (creation_date > ? OR (creation_date = ? AND key > ?))
			ORDER BY creation_date, key
			LIMIT ?`, prefix+"%", t, t, lastID, limit)
	}
	// Older Cursor: bubbles stored in ItemTable with the same key prefix.
	prefix := "bubbleId:" + composerID + ":"
	return conn.QueryContext(ctx, `
		SELECT key, value, 0 AS creation_date FROM ItemTable
		WHERE key LIKE ?
		  AND (key > ?)
		ORDER BY key
		LIMIT ?`, prefix+"%", prefix+lastID, limit)
}

// RowComplete reports whether a fetched bubble row is fully written.
// Cursor writes bubbles atomically; we treat any non-empty value as
// complete.
func (s *CursorIDESource) RowComplete(ctx context.Context, conn *sql.DB, row map[string]any) bool {
	if v, ok := row["value"]; ok {
		if b, ok := v.(string); ok && b != "" {
			return true
		}
	}
	return false
}

// RowsToMessages decodes each bubble's JSON value into a NormalizedMessage.
// Cursor bubble JSON carries {text, role, type, ...}; we extract text
// from `text` or `richText` fields and role from `role`/`type`.
func (s *CursorIDESource) RowsToMessages(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, rows []map[string]any) []ingest.NormalizedMessage {
	out := make([]ingest.NormalizedMessage, 0, len(rows))
	for _, row := range rows {
		raw, _ := row["value"].(string)
		if raw == "" {
			continue
		}
		var b map[string]any
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			continue
		}
		text := extractCursorBubbleText(b)
		if text == "" {
			continue
		}
		role := cursorBubbleRole(b)
		if role != "user" && role != "assistant" {
			continue
		}
		peer := s.assistantPeer("", "")
		if role == "user" {
			peer = s.userPeer("", "")
		}
		var ts string
		if t, ok := row["creation_date"]; ok {
			ts = ingest.ISOFromEpochMS(toInt64(t))
		}
		if ts == "" {
			ts = nowISO()
		}
		bubbleID := ""
		if k, ok := row["key"].(string); ok {
			parts := strings.Split(k, ":")
			if len(parts) > 0 {
				bubbleID = parts[len(parts)-1]
			}
		}
		out = append(out, ingest.NormalizedMessage{
			Role:      role,
			Text:      text,
			CreatedAt: ts,
			PeerID:    peer,
			Meta: map[string]any{
				"composer_id": ref.Locator,
				"bubble_id":   bubbleID,
			},
		})
	}
	return out
}

// parseCursorComposerMeta decodes a composerData JSON value and returns
// (title, started_at). Composer blobs carry {name, createdAt, ...} in
// recent Cursor versions; older versions stored an array of composer
// metadata. We handle both shapes defensively.
func parseCursorComposerMeta(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ""
	}
	// Newer: single object.
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil && obj != nil {
		title, _ := obj["name"].(string)
		if title == "" {
			title, _ = obj["title"].(string)
		}
		startedAt := ""
		if ts, ok := obj["createdAt"].(string); ok {
			startedAt = ts
		} else if ts, ok := obj["createdAt"].(float64); ok {
			startedAt = ingest.ISOFromEpochMS(int64(ts))
		}
		return title, startedAt
	}
	// Older: array of composers; pick the first.
	var arr []map[string]any
	if err := json.Unmarshal([]byte(trimmed), &arr); err == nil && len(arr) > 0 {
		obj := arr[0]
		title, _ := obj["name"].(string)
		startedAt := ""
		if ts, ok := obj["createdAt"].(string); ok {
			startedAt = ts
		} else if ts, ok := obj["createdAt"].(float64); ok {
			startedAt = ingest.ISOFromEpochMS(int64(ts))
		}
		return title, startedAt
	}
	return "", ""
}

// extractCursorBubbleText pulls the text content from a Cursor bubble
// JSON value. Bubbles carry {text, richText: [{type:"text", text:...}]}
// depending on Cursor version; we extract from either field.
func extractCursorBubbleText(b map[string]any) string {
	if t, ok := b["text"].(string); ok && strings.TrimSpace(t) != "" {
		return strings.TrimSpace(t)
	}
	if rich, ok := b["richText"].([]any); ok {
		var chunks []string
		for _, item := range rich {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] != "text" {
				continue
			}
			if t, ok := m["text"].(string); ok && strings.TrimSpace(t) != "" {
				chunks = append(chunks, strings.TrimSpace(t))
			}
		}
		if len(chunks) > 0 {
			return strings.Join(chunks, "\n")
		}
	}
	// Last resort: fullMarkdown field on some assistant bubbles.
	if t, ok := b["fullMarkdown"].(string); ok && strings.TrimSpace(t) != "" {
		return strings.TrimSpace(t)
	}
	return ""
}

// cursorBubbleRole returns the role ("user" or "assistant") for a bubble.
// Cursor uses `role` on user bubbles and `type:"assistant"` on assistant
// bubbles in some versions; we normalize both.
func cursorBubbleRole(b map[string]any) string {
	if r, ok := b["role"].(string); ok {
		switch r {
		case "user", "assistant":
			return r
		}
	}
	if t, ok := b["type"].(string); ok {
		switch t {
		case "user":
			return "user"
		case "assistant":
			return "assistant"
		}
	}
	return ""
}

// tableExists reports whether tableName exists in conn.
func tableExists(ctx context.Context, conn *sql.DB, tableName string) (bool, error) {
	var name string
	err := conn.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name=? LIMIT 1",
		tableName).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// _ = time.Second keeps the time import alive for future timestamp
// coercion helpers used by Cursor bubble decoding.
var _ = time.Second
