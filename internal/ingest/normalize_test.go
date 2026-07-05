package ingest

import "testing"

func TestToAddTurn_User(t *testing.T) {
	msg := NormalizedMessage{
		Role:      "user",
		Text:      "Hello, world",
		PeerID:    "alice",
		CreatedAt: "2024-01-01T00:00:00Z",
	}
	role, content, peerID, createdAt, ok := ToAddTurn(msg)
	if !ok {
		t.Fatal("ToAddTurn returned ok=false for user message with text")
	}
	if role != "user" {
		t.Errorf("role = %q, want %q", role, "user")
	}
	if content != "Hello, world" {
		t.Errorf("content = %q, want %q", content, "Hello, world")
	}
	if peerID != "alice" {
		t.Errorf("peerID = %q, want %q", peerID, "alice")
	}
	if createdAt != "2024-01-01T00:00:00Z" {
		t.Errorf("createdAt = %q, want %q", createdAt, "2024-01-01T00:00:00Z")
	}
}

func TestToAddTurn_Assistant(t *testing.T) {
	msg := NormalizedMessage{
		Role: "assistant",
		Text: "Hi there!",
	}
	role, _, _, _, ok := ToAddTurn(msg)
	if !ok {
		t.Fatal("ToAddTurn returned ok=false for assistant message with text")
	}
	if role != "assistant" {
		t.Errorf("role = %q, want %q", role, "assistant")
	}
}

func TestToAddTurn_EmptyText(t *testing.T) {
	msg := NormalizedMessage{
		Role: "user",
		Text: "   ",
	}
	_, _, _, _, ok := ToAddTurn(msg)
	if ok {
		t.Error("ToAddTurn returned ok=true for whitespace-only text")
	}
}

func TestToAddTurn_ToolOnlyDropped(t *testing.T) {
	msg := NormalizedMessage{
		Role:  "assistant",
		Text:  "",
		Parts: []map[string]any{{"type": "tool_use", "name": "bash"}},
	}
	_, _, _, _, ok := ToAddTurn(msg)
	if ok {
		t.Error("ToAddTurn returned ok=true for tool-only message (empty text)")
	}
}

func TestToAddTurn_UnknownRoleDefaultsUser(t *testing.T) {
	msg := NormalizedMessage{
		Role: "system",
		Text: "system prompt",
	}
	role, _, _, _, ok := ToAddTurn(msg)
	if !ok {
		t.Fatal("ToAddTurn returned ok=false for message with text")
	}
	if role != "user" {
		t.Errorf("role = %q for unknown role, want %q (default)", role, "user")
	}
}

func TestNormalizeMessages_DropsEmpty(t *testing.T) {
	msgs := []NormalizedMessage{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: ""},
		{Role: "user", Text: "  "},
		{Role: "assistant", Text: "world"},
	}
	out := NormalizeMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("NormalizeMessages returned %d messages, want 2", len(out))
	}
	if out[0].Text != "hello" {
		t.Errorf("first message text = %q, want %q", out[0].Text, "hello")
	}
	if out[1].Text != "world" {
		t.Errorf("second message text = %q, want %q", out[1].Text, "world")
	}
}

func TestNormalizeMessages_KeepsPartsWithEmptyText(t *testing.T) {
	msgs := []NormalizedMessage{
		{Role: "assistant", Text: "", Parts: []map[string]any{{"type": "tool_use"}}},
		{Role: "user", Text: "hello"},
	}
	out := NormalizeMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("NormalizeMessages returned %d messages, want 2 (parts kept)", len(out))
	}
}

func TestNormalizeMessages_EmptyInput(t *testing.T) {
	out := NormalizeMessages(nil)
	if len(out) != 0 {
		t.Errorf("NormalizeMessages(nil) returned %d, want 0", len(out))
	}
}
