package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ingest"
)

// OpenCodeSource parses sst/opencode SQLite logs.
//
// Logs: ~/.local/share/opencode/opencode.db (SQLite, WAL).
// session(id, title, directory, model, time_created);
// message(id, session_id, time_created, data JSON {role, modelID, providerID, ...});
// the actual text lives in part(message_id, time_created, data JSON{type, text}).
// Polled read-only via (time_created, id) cursor.
//
// Older OpenCode versions used a JSON file-store; that layout is not
// supported by this adapter (the SQLite schema is the canonical one).
type OpenCodeSource struct {
	SqliteLogSource
}

func init() {
	ingest.Register("opencode", func(cfg ingest.SourceConfig) (ingest.Source, error) {
		src := NewOpenCodeSource()
		if len(cfg.Paths) > 0 {
			src.Paths = cfg.Paths
		}
		if cfg.User != "" {
			src.FallbackUser = cfg.User
		}
		return src, nil
	})
}

// NewOpenCodeSource constructs an OpenCodeSource with default roots.
func NewOpenCodeSource() *OpenCodeSource {
	src := &OpenCodeSource{
		SqliteLogSource: SqliteLogSource{
			Name:         "opencode",
			Paths:        []string{"~/.local/share/opencode/opencode.db"},
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
func (s *OpenCodeSource) DBPath() string {
	roots := s.roots()
	if len(roots) > 0 {
		return expandTilde(roots[0])
	}
	return expandTilde("~/.local/share/opencode/opencode.db")
}

// Discover enumerates OpenCode sessions from the SQLite DB.
func (s *OpenCodeSource) Discover(ctx context.Context, conn *sql.DB, fn func(ingest.SessionRef) error) error {
	rows, err := conn.QueryContext(ctx,
		"SELECT id, title, directory, model, time_created FROM session ORDER BY time_created")
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	defer rows.Close()
	for rows.Next() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var id, title, directory, model sql.NullString
		var timeCreated sql.NullInt64
		if err := rows.Scan(&id, &title, &directory, &model, &timeCreated); err != nil {
			continue
		}
		if !id.Valid || id.String == "" {
			continue
		}
		ref := ingest.SessionRef{
			Harness:         s.Name,
			NativeSessionID: id.String,
			Locator:         id.String,
			Title:           title.String,
			StartedAt:       ingest.ISOFromEpochMS(timeCreated.Int64),
			Meta: map[string]any{
				"cwd":           directory.String,
				"session_model": model.String,
			},
		}
		if err := fn(ref); err != nil {
			return err
		}
	}
	return rows.Err()
}

// FetchRows returns up to limit message rows after cursor.
func (s *OpenCodeSource) FetchRows(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, cursor *ingest.Cursor, limit int) (*sql.Rows, error) {
	var t int64
	var lastID string
	if cursor != nil {
		t = toInt64(cursor.Value["time"])
		if id, ok := cursor.Value["id"].(string); ok {
			lastID = id
		}
	}
	return conn.QueryContext(ctx, `
        SELECT id, time_created, data FROM message
        WHERE session_id = ?
          AND (time_created > ? OR (time_created = ? AND id > ?))
        ORDER BY time_created, id
        LIMIT ?`, ref.Locator, t, t, lastID, limit)
}

// RowComplete reports whether a fetched message row is fully flushed
// (its `part` text has landed). User rows are always complete; assistant
// rows are complete once data.time.completed or data.finish is set.
func (s *OpenCodeSource) RowComplete(ctx context.Context, conn *sql.DB, row map[string]any) bool {
	raw, ok := row["data"].(string)
	if !ok {
		return true
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return true
	}
	if data["role"] == "user" {
		return true
	}
	if tm, ok := data["time"].(map[string]any); ok {
		if _, ok := tm["completed"]; ok {
			return true
		}
	}
	if _, ok := data["finish"]; ok {
		return true
	}
	return false
}

// RowsToMessages reassembles each message's text from the part table.
func (s *OpenCodeSource) RowsToMessages(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, rows []map[string]any) []ingest.NormalizedMessage {
	out := make([]ingest.NormalizedMessage, 0, len(rows))
	for _, row := range rows {
		raw, ok := row["data"].(string)
		if !ok {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			continue
		}
		role, _ := data["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		messageID, _ := row["id"].(string)
		text := s.reassembleText(ctx, conn, messageID)
		if text == "" {
			continue
		}
		modelID, _ := data["modelID"].(string)
		providerID, _ := data["providerID"].(string)
		cwd, _ := ref.Meta["cwd"].(string)
		peer := s.assistantPeer(modelID, providerID)
		if role == "user" {
			peer = s.userPeer(cwd, "")
		}
		if modelID == "" {
			if sm, ok := ref.Meta["session_model"].(string); ok {
				modelID = sm
			}
		}
		out = append(out, ingest.NormalizedMessage{
			Role:      role,
			Text:      text,
			CreatedAt: ingest.ISOFromEpochMS(row["time_created"]),
			PeerID:    peer,
			Meta: map[string]any{
				"model":    modelID,
				"provider": providerID,
				"cwd":      cwd,
			},
		})
	}
	return out
}

// reassembleText concatenates the text parts of a message ordered by
// (time_created, id).
func (s *OpenCodeSource) reassembleText(ctx context.Context, conn *sql.DB, messageID string) string {
	if messageID == "" {
		return ""
	}
	rows, err := conn.QueryContext(ctx,
		"SELECT data FROM part WHERE message_id = ? ORDER BY time_created, id",
		messageID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var chunks []string
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		if !raw.Valid || raw.String == "" {
			continue
		}
		var part map[string]any
		if err := json.Unmarshal([]byte(raw.String), &part); err != nil {
			continue
		}
		if part["type"] != "text" {
			continue
		}
		if t, ok := part["text"].(string); ok {
			if strings.TrimSpace(t) != "" {
				chunks = append(chunks, strings.TrimSpace(t))
			}
		}
	}
	return strings.TrimSpace(strings.Join(chunks, "\n"))
}
