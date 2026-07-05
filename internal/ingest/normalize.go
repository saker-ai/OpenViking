package ingest

import "strings"

// ToAddTurn converts a NormalizedMessage into a (role, content, peer_id,
// created_at) tuple suitable for session.Store.AppendTurn. Returns
// ok=false when the message has no replayable text (a tool-only turn).
//
// Mirrors openviking/ingest/normalize.to_add_message_request: conversation
// memory is driven by user/assistant TEXT; tool I/O is dropped as
// low-value.
func ToAddTurn(msg NormalizedMessage) (role, content, peerID, createdAt string, ok bool) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return "", "", "", "", false
	}
	role = "user"
	if msg.Role == "assistant" {
		role = "assistant"
	}
	return role, text, SafePeerID(msg.PeerID), msg.CreatedAt, true
}

// NormalizeMessages drops empty turns from a batch and returns the
// replayable subset. Callers iterate the result to call AppendTurn.
func NormalizeMessages(messages []NormalizedMessage) []NormalizedMessage {
	out := make([]NormalizedMessage, 0, len(messages))
	for _, m := range messages {
		if strings.TrimSpace(m.Text) == "" && len(m.Parts) == 0 {
			continue
		}
		out = append(out, m)
	}
	return out
}
