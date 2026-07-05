// Package session is the bot-side adapter for the OpenViking session
// store. It wraps internal/session.Store so the agent loop can:
//
//   - Resolve or create a session for an (account, user, peer) tuple
//   - Append user / assistant / tool turns
//   - Commit & archive on conversation end
//
// The adapter is a thin pass-through; it does not duplicate logic from
// internal/session. It exists so the bot package does not import
// internal/session directly (which would couple the agent loop to the
// store's internal types).
package session

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/saker-ai/ctxhub/internal/bot/channels"
	"github.com/saker-ai/ctxhub/internal/domain"
	isession "github.com/saker-ai/ctxhub/internal/session"
)

// Adapter wraps a session.Store with bot-friendly helpers.
type Adapter struct {
	store isession.Store
}

// New returns an Adapter over store.
func New(store isession.Store) *Adapter {
	return &Adapter{store: store}
}

// EnsureSession returns the active session for the given channel
// message. When no active session exists for the (account, user, peer)
// tuple, a new one is created. The session ID is returned.
//
// The peer is derived from the channel name (e.g. "telegram") so each
// platform's conversation is isolated.
func (a *Adapter) EnsureSession(ctx context.Context, msg channels.IncomingMessage) (string, error) {
	id := msg.Identity
	if id.Account == "" {
		return "", fmt.Errorf("session: identity account is empty")
	}
	if id.User == "" {
		id.User = msg.UserID
	}
	if id.ActorPeer == "" {
		id.ActorPeer = msg.ChannelName
	}
	// Find an active session for this identity. We use List and pick
	// the most recent active one; the store returns sessions ordered
	// by CreatedAt descending.
	list, err := a.store.List(ctx, id)
	if err != nil {
		return "", fmt.Errorf("session: list: %w", err)
	}
	for _, s := range list {
		if s.Status == domain.SessionStatusActive {
			return s.ID, nil
		}
	}
	sess, err := a.store.Create(ctx, id)
	if err != nil {
		return "", fmt.Errorf("session: create: %w", err)
	}
	return sess.ID, nil
}

// AppendUserTurn appends a user turn to sessionID.
func (a *Adapter) AppendUserTurn(ctx context.Context, msg channels.IncomingMessage, sessionID string) error {
	turn := domain.Turn{
		ID:        uuid.NewString(),
		Role:      domain.TurnRoleUser,
		Content:   msg.Text,
		CreatedAt: time.Now(),
	}
	return a.store.AppendTurn(ctx, msg.Identity, sessionID, turn)
}

// AppendAssistantTurn appends an assistant turn (text and/or tool calls).
func (a *Adapter) AppendAssistantTurn(ctx context.Context, id domain.Identifier, sessionID, content string, toolCalls []domain.ToolCall) error {
	turn := domain.Turn{
		ID:        uuid.NewString(),
		Role:      domain.TurnRoleAssistant,
		Content:   content,
		ToolCalls: toolCalls,
		CreatedAt: time.Now(),
	}
	return a.store.AppendTurn(ctx, id, sessionID, turn)
}

// AppendToolResultTurn appends a tool-result turn for the given call ID.
func (a *Adapter) AppendToolResultTurn(ctx context.Context, id domain.Identifier, sessionID, callID, result string) error {
	turn := domain.Turn{
		ID:        uuid.NewString(),
		Role:      domain.TurnRoleTool,
		Content:   result,
		CreatedAt: time.Now(),
	}
	// The originating call ID is encoded in the turn's ToolCalls slice
	// as a single entry with the call ID and the result, so consumers
	// can correlate the tool-result turn with the prior assistant call
	// without adding a new field to the domain type.
	if callID != "" {
		turn.ToolCalls = []domain.ToolCall{{ID: callID, Result: result}}
	}
	return a.store.AppendTurn(ctx, id, sessionID, turn)
}

// Commit persists the session and runs the configured compressor.
func (a *Adapter) Commit(ctx context.Context, id domain.Identifier, sessionID string) (*domain.Session, error) {
	return a.store.Commit(ctx, id, sessionID)
}

// Archive transitions the session to archived (terminal).
func (a *Adapter) Archive(ctx context.Context, id domain.Identifier, sessionID string) error {
	return a.store.Archive(ctx, id, sessionID)
}

// Get returns the session by ID.
func (a *Adapter) Get(ctx context.Context, id domain.Identifier, sessionID string) (*domain.Session, error) {
	return a.store.Get(ctx, id, sessionID)
}
