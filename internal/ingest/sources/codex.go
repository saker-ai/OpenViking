package sources

import (
	"strings"

	"github.com/saker-ai/ctxhub/internal/ingest"
)

// CodexSource parses OpenAI Codex CLI JSONL rollout files.
//
// Logs: ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
// Records: {timestamp, type, payload}. Conversation turns are
// type=="response_item" & payload.type=="message" with payload.role and
// payload.content[].text (input_text / output_text). role=="developer" /
// "system" are dropped. Session id / cwd / provider come from the first
// session_meta record.
type CodexSource struct {
	JsonlLogSource
}

func init() {
	ingest.Register("codex", func(cfg ingest.SourceConfig) (ingest.Source, error) {
		src := NewCodexSource()
		if len(cfg.Paths) > 0 {
			src.Paths = cfg.Paths
		}
		if cfg.User != "" {
			src.FallbackUser = cfg.User
		}
		return src, nil
	})
}

// NewCodexSource constructs a CodexSource with default roots.
func NewCodexSource() *CodexSource {
	src := &CodexSource{
		JsonlLogSource: JsonlLogSource{
			Name:         "codex",
			FileGlob:     "*/*/*/rollout-*.jsonl",
			Paths:        []string{"~/.codex/sessions"},
			FallbackUser: "default",
		},
	}
	src.parseLine = src.ParseLine
	src.sessionMeta = src.SessionMeta
	return src
}

// SessionMeta peeks the first session_meta record to populate
// native_session_id, started_at, model_provider, and cwd.
func (s *CodexSource) SessionMeta(path string) map[string]any {
	first := peekFirstJSON(path)
	if first == nil {
		return nil
	}
	if first["type"] != "session_meta" {
		return nil
	}
	payload, _ := first["payload"].(map[string]any)
	if payload == nil {
		return nil
	}
	out := map[string]any{}
	if id, ok := payload["id"].(string); ok {
		out["id"] = id
	}
	if ts, ok := payload["timestamp"]; ok {
		out["timestamp"] = toString(ts)
	}
	if mp, ok := payload["model_provider"].(string); ok {
		out["model"] = mp
	}
	if cwd, ok := payload["cwd"].(string); ok {
		out["cwd"] = cwd
	}
	return out
}

// ParseLine maps one Codex JSONL record to zero or more messages.
func (s *CodexSource) ParseLine(obj map[string]any, ref ingest.SessionRef) []ingest.NormalizedMessage {
	if obj["type"] != "response_item" {
		return nil
	}
	payload, _ := obj["payload"].(map[string]any)
	if payload == nil || payload["type"] != "message" {
		return nil
	}
	role, _ := payload["role"].(string)
	if role != "user" && role != "assistant" {
		return nil
	}
	text := codexJoinContent(payload["content"])
	if text == "" {
		return nil
	}
	model, _ := ref.Meta["model"].(string)
	cwd, _ := ref.Meta["cwd"].(string)
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
			"model": model,
			"cwd":   cwd,
		},
	}}
}

// codexJoinContent pulls text from a Codex payload.content list, accepting
// input_text and output_text blocks. Returns the joined, trimmed text.
func codexJoinContent(content any) string {
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	var chunks []string
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		if t != "input_text" && t != "output_text" {
			continue
		}
		text, _ := m["text"].(string)
		if strings.TrimSpace(text) != "" {
			chunks = append(chunks, strings.TrimSpace(text))
		}
	}
	return strings.TrimSpace(strings.Join(chunks, "\n"))
}
