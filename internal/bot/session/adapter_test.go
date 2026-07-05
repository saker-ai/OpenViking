package session

import (
	"context"
	"testing"

	"github.com/saker-ai/ctxhub/internal/bot/channels"
	"github.com/saker-ai/ctxhub/internal/domain"
	isession "github.com/saker-ai/ctxhub/internal/session"
)

func TestAdapter_EnsureSessionCreates(t *testing.T) {
	store := isession.NewMemoryStore(isession.StoreConfig{})
	a := New(store)
	ctx := context.Background()

	msg := channels.IncomingMessage{
		ChannelName: "telegram",
		ChatID:      "1",
		UserID:      "u1",
		Text:        "hi",
		Identity:    domain.Identifier{Account: "acct", User: "u1", ActorPeer: "telegram"},
	}
	id, err := a.EnsureSession(ctx, msg)
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if id == "" {
		t.Fatalf("session ID empty")
	}
	// Second EnsureSession should return the same ID (active session reused).
	id2, err := a.EnsureSession(ctx, msg)
	if err != nil {
		t.Fatalf("EnsureSession 2: %v", err)
	}
	if id2 != id {
		t.Errorf("EnsureSession 2 = %q, want %q (active session reused)", id2, id)
	}
}

func TestAdapter_EnsureSessionRequiresAccount(t *testing.T) {
	store := isession.NewMemoryStore(isession.StoreConfig{})
	a := New(store)
	_, err := a.EnsureSession(context.Background(), channels.IncomingMessage{})
	if err == nil {
		t.Fatalf("EnsureSession should error on empty account")
	}
}

func TestAdapter_AppendTurns(t *testing.T) {
	store := isession.NewMemoryStore(isession.StoreConfig{})
	a := New(store)
	ctx := context.Background()
	msg := channels.IncomingMessage{
		ChannelName: "telegram",
		ChatID:      "1",
		UserID:      "u1",
		Text:        "hello",
		Identity:    domain.Identifier{Account: "acct", User: "u1", ActorPeer: "telegram"},
	}
	id, _ := a.EnsureSession(ctx, msg)
	if err := a.AppendUserTurn(ctx, msg, id); err != nil {
		t.Fatalf("AppendUserTurn: %v", err)
	}
	if err := a.AppendAssistantTurn(ctx, msg.Identity, id, "hi back", nil); err != nil {
		t.Fatalf("AppendAssistantTurn: %v", err)
	}
	if err := a.AppendToolResultTurn(ctx, msg.Identity, id, "call-1", "result-data"); err != nil {
		t.Fatalf("AppendToolResultTurn: %v", err)
	}
	sess, err := a.Get(ctx, msg.Identity, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(sess.Turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(sess.Turns))
	}
	if sess.Turns[0].Role != domain.TurnRoleUser {
		t.Errorf("turn 0 role = %q", sess.Turns[0].Role)
	}
	if sess.Turns[1].Role != domain.TurnRoleAssistant {
		t.Errorf("turn 1 role = %q", sess.Turns[1].Role)
	}
	if sess.Turns[2].Role != domain.TurnRoleTool {
		t.Errorf("turn 2 role = %q", sess.Turns[2].Role)
	}
	if sess.Turns[2].ToolCalls[0].ID != "call-1" {
		t.Errorf("tool call id = %q", sess.Turns[2].ToolCalls[0].ID)
	}
}

func TestAdapter_CommitAndArchive(t *testing.T) {
	store := isession.NewMemoryStore(isession.StoreConfig{})
	a := New(store)
	ctx := context.Background()
	msg := channels.IncomingMessage{
		ChannelName: "telegram",
		ChatID:      "1",
		UserID:      "u1",
		Text:        "hi",
		Identity:    domain.Identifier{Account: "acct", User: "u1", ActorPeer: "telegram"},
	}
	id, _ := a.EnsureSession(ctx, msg)
	_ = a.AppendUserTurn(ctx, msg, id)
	if _, err := a.Commit(ctx, msg.Identity, id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := a.Archive(ctx, msg.Identity, id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	sess, _ := a.Get(ctx, msg.Identity, id)
	if sess.Status != domain.SessionStatusArchived {
		t.Errorf("status = %q, want archived", sess.Status)
	}
}
