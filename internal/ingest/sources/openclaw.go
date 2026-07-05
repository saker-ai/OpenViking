package sources

import (
	"github.com/saker-ai/ctxhub/internal/ingest"
)

// OpenClawSource parses OpenClaw (group-chat agent) JSONL session logs.
//
// Logs: ~/.openclaw/agents/<agent>/sessions/<uuid>.jsonl. Records carry a
// top-level "type"; conversation turns are type=="message" with a nested
// message{role, content[], timestamp}. Assistant messages additionally
// carry "model" + "provider". content is a list of blocks (text / thinking).
//
// Group-chat agent: user turns map to the original username when present
// (configured via UserField in Settings); otherwise the configured OV user.
type OpenClawSource struct {
	JsonlLogSource
	// UserField is the JSON key carrying the original username in the
	// nested message. Defaults to "user".
	UserField string
}

func init() {
	ingest.Register("openclaw", func(cfg ingest.SourceConfig) (ingest.Source, error) {
		src := NewOpenClawSource()
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

// NewOpenClawSource constructs an OpenClawSource with default roots.
func NewOpenClawSource() *OpenClawSource {
	src := &OpenClawSource{
		JsonlLogSource: JsonlLogSource{
			Name:         "openclaw",
			FileGlob:     "*/sessions/*.jsonl",
			Paths:        []string{"~/.openclaw/agents"},
			GroupChat:    true,
			FallbackUser: "default",
		},
		UserField: "user",
	}
	src.parseLine = src.ParseLine
	return src
}

// ParseLine maps one OpenClaw JSONL record to zero or more messages.
func (s *OpenClawSource) ParseLine(obj map[string]any, ref ingest.SessionRef) []ingest.NormalizedMessage {
	if obj["type"] != "message" {
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
	provider, _ := message["provider"].(string)
	peer := s.assistantPeer(model, provider)
	if role == "user" {
		rawUser, _ := message[s.UserField].(string)
		peer = s.userPeer("", rawUser)
	}
	return []ingest.NormalizedMessage{{
		Role:      role,
		Text:      text,
		CreatedAt: toString(obj["timestamp"]),
		PeerID:    peer,
		Meta: map[string]any{
			"model":    model,
			"provider": provider,
		},
	}}
}
