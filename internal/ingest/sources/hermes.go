package sources

import (
	"strings"

	"github.com/saker-ai/ctxhub/internal/ingest"
)

// HermesSource parses Hermes (group-chat agent) JSONL session logs.
//
// Logs: ~/.hermes/sessions/<ts>_<id>.jsonl. Records are keyed by "role":
// a leading session_meta (carries "model" + "platform"), then "user" /
// "assistant" turns with "content" (str) + "timestamp".
//
// Group-chat agent: user turns map to the original username when present
// (configured via UserField in Settings); otherwise the configured OV user.
type HermesSource struct {
	JsonlLogSource
	// UserField is the JSON key carrying the original username. Defaults
	// to "user".
	UserField string
}

func init() {
	ingest.Register("hermes", func(cfg ingest.SourceConfig) (ingest.Source, error) {
		src := NewHermesSource()
		if len(cfg.Paths) > 0 {
			src.Paths = cfg.Paths
		}
		if cfg.User != "" {
			src.FallbackUser = cfg.User
		}
		if uf, ok := cfg.Settings["user_field"].(string); ok && uf != "" {
			src.UserField = uf
		}
		return src, nil
	})
}

// NewHermesSource constructs a HermesSource with default roots.
func NewHermesSource() *HermesSource {
	src := &HermesSource{
		JsonlLogSource: JsonlLogSource{
			Name:         "hermes",
			FileGlob:     "*.jsonl",
			Paths:        []string{"~/.hermes/sessions"},
			GroupChat:    true,
			FallbackUser: "default",
		},
		UserField: "user",
	}
	src.parseLine = src.ParseLine
	src.sessionMeta = src.SessionMeta
	return src
}

// SessionMeta peeks the first record for session-level model + platform.
func (s *HermesSource) SessionMeta(path string) map[string]any {
	first := peekFirstJSON(path)
	if first == nil {
		return nil
	}
	out := map[string]any{}
	if first["role"] == "session_meta" {
		if m, ok := first["model"].(string); ok {
			out["model"] = m
		}
		if p, ok := first["platform"].(string); ok {
			out["platform"] = p
		}
		if ts, ok := first["timestamp"]; ok {
			out["timestamp"] = toString(ts)
		}
	}
	return out
}

// ParseLine maps one Hermes JSONL record to zero or more messages.
func (s *HermesSource) ParseLine(obj map[string]any, ref ingest.SessionRef) []ingest.NormalizedMessage {
	role, _ := obj["role"].(string)
	if role != "user" && role != "assistant" {
		return nil
	}
	content, _ := obj["content"].(string)
	text := strings.TrimSpace(content)
	if text == "" {
		return nil
	}
	model, _ := ref.Meta["model"].(string)
	platform, _ := ref.Meta["platform"].(string)
	peer := s.assistantPeer(model, "")
	if role == "user" {
		rawUser, _ := obj[s.UserField].(string)
		peer = s.userPeer("", rawUser)
	}
	return []ingest.NormalizedMessage{{
		Role:      role,
		Text:      text,
		CreatedAt: toString(obj["timestamp"]),
		PeerID:    peer,
		Meta: map[string]any{
			"model":    model,
			"platform": platform,
		},
	}}
}
