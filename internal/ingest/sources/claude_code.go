package sources

import (
	"encoding/json"
	"os"

	"github.com/saker-ai/ctxhub/internal/ingest"
)

// ClaudeCodeSource parses Claude Code JSONL transcript files.
//
// Logs: ~/.claude/projects/<project-slug>/<session-uuid>.jsonl
// Each record has a top-level "type"; conversation turns are
// type in {user, assistant} with a nested message{role, content, model}.
// content is a string or a list of blocks (text / tool_use / tool_result).
// cwd + gitBranch are per record.
type ClaudeCodeSource struct {
	JsonlLogSource
}

func init() {
	ingest.Register("claude_code", func(cfg ingest.SourceConfig) (ingest.Source, error) {
		src := NewClaudeCodeSource()
		if len(cfg.Paths) > 0 {
			src.Paths = cfg.Paths
		}
		if cfg.User != "" {
			src.FallbackUser = cfg.User
		}
		return src, nil
	})
}

// NewClaudeCodeSource constructs a ClaudeCodeSource with default roots.
func NewClaudeCodeSource() *ClaudeCodeSource {
	src := &ClaudeCodeSource{
		JsonlLogSource: JsonlLogSource{
			Name:         "claude_code",
			FileGlob:     "*/*.jsonl",
			Paths:        []string{"~/.claude/projects"},
			FallbackUser: "default",
		},
	}
	src.parseLine = src.ParseLine
	return src
}

// ParseLine maps one Claude Code JSONL record to zero or more messages.
func (s *ClaudeCodeSource) ParseLine(obj map[string]any, ref ingest.SessionRef) []ingest.NormalizedMessage {
	t, _ := obj["type"].(string)
	if t != "user" && t != "assistant" {
		return nil
	}
	if isTruthy(obj["isSidechain"]) || isTruthy(obj["isMeta"]) {
		return nil
	}
	message, _ := obj["message"].(map[string]any)
	if message == nil {
		return nil
	}
	role, _ := message["role"].(string)
	if role != "user" && role != "assistant" {
		return nil
	}
	text := extractTextFromContent(message["content"])
	if text == "" {
		return nil
	}
	model, _ := message["model"].(string)
	cwd, _ := obj["cwd"].(string)
	peer := s.assistantPeer(model, "")
	if role == "user" {
		peer = s.userPeer(cwd, "")
	}
	return []ingest.NormalizedMessage{{
		Role:      role,
		Text:      text,
		CreatedAt: toString(obj["timestamp"]),
		PeerID:    peer,
		Meta: map[string]any{
			"model":      model,
			"cwd":        cwd,
			"git_branch": obj["gitBranch"],
		},
	}}
}

// toString coerces an any to its string form. Used for timestamp fields
// that may arrive as string or numeric.
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return ingest.ISOFromEpochMS(x)
	case int64:
		return ingest.ISOFromEpochMS(x)
	case int:
		return ingest.ISOFromEpochMS(x)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// isTruthy reports whether v is truthy (non-zero / non-empty / non-false).
func isTruthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != "" && x != "false" && x != "0"
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case nil:
		return false
	}
	return false
}

// peekFirstJSON reads the first non-blank JSON object from path. Returns
// nil on read error or empty file. Used by adapters that store session
// metadata in the leading record.
func peekFirstJSON(path string) map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			return nil
		}
		if obj != nil {
			return obj
		}
	}
}
