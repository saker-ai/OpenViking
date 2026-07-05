package session

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// Store is the persistence boundary for Session entities and their extracted
// memory. Implementations may live in memory (tests, dev) or on top of ragfs
// (production). All methods are scoped by the caller's Identifier so that
// multi-tenant isolation is enforced at every layer.
//
// Lifecycle:
//
//	Create          -> status=active, turns=[]
//	AppendTurn      -> appends to active session, updates TokenUsage
//	Commit          -> status=committed, sets CommittedAt, runs compressor
//	Archive         -> status=archived (terminal)
//	ExtractMemory   -> reads the session's currently stored memory
//	ApplyMemoryDiff -> mutates the session's stored memory (add/update/archive)
type Store interface {
	// Create allocates a new active Session for the given identity. The
	// returned Session.ID is unique within the (account, user, peer)
	// namespace.
	Create(ctx context.Context, id domain.Identifier) (*domain.Session, error)

	// CreateWithID allocates a new active Session with a caller-supplied
	// session ID. When sessionID is empty, falls back to a server-generated
	// ID (equivalent to Create). When sessionID is non-empty and a session
	// with that ID already exists for the caller's identity, returns
	// domain.ErrConflict. This is the SDK-compat path: the Go SDK
	// (sdk/go) posts {"session_id":"..."} and expects the server to honor
	// the caller-supplied ID so subsequent GetSession/AddMessage/etc.
	// calls against that ID resolve correctly.
	CreateWithID(ctx context.Context, id domain.Identifier, sessionID string) (*domain.Session, error)

	// Get retrieves a Session by ID. Returns domain.ErrNotFound when the
	// session does not exist or belongs to a different tenant.
	Get(ctx context.Context, id domain.Identifier, sessionID string) (*domain.Session, error)

	// AppendTurn appends a Turn to an active session. The turn's ID and
	// CreatedAt are populated if zero. TokenUsage is updated.
	AppendTurn(ctx context.Context, id domain.Identifier, sessionID string, turn domain.Turn) error

	// List returns all sessions for the identity ordered by CreatedAt
	// descending. Archived sessions may be filtered by the implementation.
	List(ctx context.Context, id domain.Identifier) ([]*domain.Session, error)

	// Commit transitions a session to committed status, sets CommittedAt,
	// and runs the configured compressor (if any) to produce a Summary.
	// The returned Session reflects the post-commit state.
	Commit(ctx context.Context, id domain.Identifier, sessionID string) (*domain.Session, error)

	// Archive transitions a committed session to archived status. Active
	// sessions may be archived directly (force-archive). Returns
	// domain.ErrNotFound if the session is missing.
	Archive(ctx context.Context, id domain.Identifier, sessionID string) error

	// Delete removes a session entirely (any status). Returns
	// domain.ErrNotFound if the session is missing. This is the SDK-compat
	// path for DELETE /api/v1/sessions/:id — the Go SDK's DeleteSession
	// expects the session to be removed, not just archived.
	Delete(ctx context.Context, id domain.Identifier, sessionID string) error

	// ExtractMemory returns the memory currently persisted on the session.
	// When the store is configured with a MemoryExtractor, it runs LLM
	// extraction first and persists the result; otherwise it returns the
	// currently stored memory without invoking the LLM.
	ExtractMemory(ctx context.Context, id domain.Identifier, sessionID string) ([]domain.ExtractedMemory, error)

	// ApplyMemoryDiff applies an additive / mutating diff to the session's
	// stored memory. IDs in Archived are removed from the session's Memory
	// slice; items in Added are appended; items in Updated replace
	// same-ID entries in place.
	ApplyMemoryDiff(ctx context.Context, id domain.Identifier, sessionID string, diff domain.MemoryDiff) error
}

// StoreConfig configures the in-memory Store implementation.
type StoreConfig struct {
	// Compressor runs during Commit. nil disables compression.
	Compressor Compressor
	// Extractor runs during ExtractMemory. nil disables LLM-driven
	// extraction; ExtractMemory then returns the stored Memory slice.
	Extractor *MemoryExtractor
}

// MemoryStore is a goroutine-safe in-memory Store. It is the reference
// implementation for tests and for early bootstrap before the ragfs-backed
// store lands. Sessions are keyed by (account, user, peer, sessionID).
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]*domain.Session
	cfg      StoreConfig
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore(cfg StoreConfig) *MemoryStore {
	return &MemoryStore{
		sessions: make(map[string]*domain.Session),
		cfg:      cfg,
	}
}

// Create implements Store.
func (s *MemoryStore) Create(ctx context.Context, id domain.Identifier) (*domain.Session, error) {
	sess := &domain.Session{
		ID:        newSessionID(),
		Account:   id.Account,
		User:      id.User,
		Peer:      id.ActorPeer,
		Status:    domain.SessionStatusActive,
		Turns:     nil,
		CreatedAt: now(),
	}
	s.mu.Lock()
	s.sessions[s.key(id, sess.ID)] = sess
	s.mu.Unlock()
	return cloneSession(sess), nil
}

// CreateWithID implements Store. When sessionID is empty, behaves like
// Create. When non-empty, uses the caller-supplied ID and returns
// domain.ErrConflict if a session with that ID already exists for the
// caller's identity.
func (s *MemoryStore) CreateWithID(ctx context.Context, id domain.Identifier, sessionID string) (*domain.Session, error) {
	if sessionID == "" {
		return s.Create(ctx, id)
	}
	sess := &domain.Session{
		ID:        sessionID,
		Account:   id.Account,
		User:      id.User,
		Peer:      id.ActorPeer,
		Status:    domain.SessionStatusActive,
		Turns:     nil,
		CreatedAt: now(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[s.key(id, sessionID)]; exists {
		return nil, domain.ErrConflict
	}
	s.sessions[s.key(id, sessionID)] = sess
	return cloneSession(sess), nil
}

// Get implements Store.
func (s *MemoryStore) Get(ctx context.Context, id domain.Identifier, sessionID string) (*domain.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[s.key(id, sessionID)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return cloneSession(sess), nil
}

// AppendTurn implements Store.
func (s *MemoryStore) AppendTurn(ctx context.Context, id domain.Identifier, sessionID string, turn domain.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[s.key(id, sessionID)]
	if !ok {
		return domain.ErrNotFound
	}
	if sess.Status == domain.SessionStatusArchived {
		return fmt.Errorf("session archived: %w", domain.ErrConflict)
	}
	if turn.ID == "" {
		turn.ID = newTurnID()
	}
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = now()
	}
	sess.Turns = append(sess.Turns, turn)
	sess.TokenUsage.PromptTokens += turn.Tokens
	sess.TokenUsage.CompletionTokens += turn.Tokens
	sess.TokenUsage.TotalTokens = sess.TokenUsage.PromptTokens + sess.TokenUsage.CompletionTokens
	return nil
}

// List implements Store.
func (s *MemoryStore) List(ctx context.Context, id domain.Identifier) ([]*domain.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Session, 0)
	for _, sess := range s.sessions {
		if sess.Account != id.Account || sess.User != id.User || sess.Peer != id.ActorPeer {
			continue
		}
		out = append(out, cloneSession(sess))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Commit implements Store.
func (s *MemoryStore) Commit(ctx context.Context, id domain.Identifier, sessionID string) (*domain.Session, error) {
	s.mu.Lock()
	sess, ok := s.sessions[s.key(id, sessionID)]
	if !ok {
		s.mu.Unlock()
		return nil, domain.ErrNotFound
	}
	if sess.Status == domain.SessionStatusArchived {
		s.mu.Unlock()
		return nil, fmt.Errorf("session archived: %w", domain.ErrConflict)
	}
	sess.Status = domain.SessionStatusCommitted
	t := now()
	sess.CommittedAt = &t
	// Clone under the lock so the compressor sees a stable snapshot.
	snapshot := cloneSession(sess)
	s.mu.Unlock()

	if s.cfg.Compressor != nil {
		compressed, err := s.cfg.Compressor.Compress(ctx, snapshot)
		if err != nil {
			// Compression failure is non-fatal to status change but is
			// surfaced to the caller so they can retry or fall back.
			return nil, err
		}
		s.mu.Lock()
		// Re-fetch in case the session was concurrently mutated.
		if cur, ok := s.sessions[s.key(id, sessionID)]; ok {
			cur.Summary = compressed.Summary
			cur.Turns = compressed.Turns
			cur.TokenUsage = compressed.TokenUsage
		}
		s.mu.Unlock()
		snapshot = compressed
	}
	return snapshot, nil
}

// Archive implements Store.
func (s *MemoryStore) Archive(ctx context.Context, id domain.Identifier, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[s.key(id, sessionID)]
	if !ok {
		return domain.ErrNotFound
	}
	sess.Status = domain.SessionStatusArchived
	return nil
}

// Delete implements Store. Removes the session entirely (any status).
func (s *MemoryStore) Delete(ctx context.Context, id domain.Identifier, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.key(id, sessionID)
	if _, ok := s.sessions[key]; !ok {
		return domain.ErrNotFound
	}
	delete(s.sessions, key)
	return nil
}

// ExtractMemory implements Store. When the store has an extractor configured
// and the session has turns, it runs LLM extraction and persists the result
// before returning. Otherwise it returns the currently stored memory.
func (s *MemoryStore) ExtractMemory(ctx context.Context, id domain.Identifier, sessionID string) ([]domain.ExtractedMemory, error) {
	s.mu.RLock()
	sess, ok := s.sessions[s.key(id, sessionID)]
	if !ok {
		s.mu.RUnlock()
		return nil, domain.ErrNotFound
	}
	snapshot := cloneSession(sess)
	s.mu.RUnlock()

	if s.cfg.Extractor != nil && len(snapshot.Turns) > 0 {
		mem, err := s.cfg.Extractor.Extract(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if cur, ok := s.sessions[s.key(id, sessionID)]; ok {
			cur.Memory = mem
		}
		s.mu.Unlock()
		return mem, nil
	}
	return snapshot.Memory, nil
}

// ApplyMemoryDiff implements Store.
func (s *MemoryStore) ApplyMemoryDiff(ctx context.Context, id domain.Identifier, sessionID string, diff domain.MemoryDiff) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[s.key(id, sessionID)]
	if !ok {
		return domain.ErrNotFound
	}
	applyDiff(sess, diff)
	return nil
}

// key returns the storage key for a session.
func (s *MemoryStore) key(id domain.Identifier, sessionID string) string {
	return id.Account + "/" + id.User + "/" + id.ActorPeer + "/" + sessionID
}

// applyDiff mutates sess.Memory in place to reflect diff. It is package-level
// so tests and future ragfs-backed stores can reuse the semantics.
func applyDiff(sess *domain.Session, diff domain.MemoryDiff) {
	// Archive: remove by ID.
	if len(diff.Archived) > 0 {
		archivedSet := make(map[string]struct{}, len(diff.Archived))
		for _, mid := range diff.Archived {
			archivedSet[mid] = struct{}{}
		}
		filtered := sess.Memory[:0]
		for _, m := range sess.Memory {
			if _, drop := archivedSet[m.ID]; drop {
				continue
			}
			filtered = append(filtered, m)
		}
		sess.Memory = filtered
	}
	// Update: replace in place by ID; items whose ID is absent become adds.
	if len(diff.Updated) > 0 {
		byID := make(map[string]int, len(sess.Memory))
		for i, m := range sess.Memory {
			byID[m.ID] = i
		}
		for _, upd := range diff.Updated {
			if idx, ok := byID[upd.ID]; ok {
				sess.Memory[idx] = upd
			} else {
				if upd.ID == "" {
					upd.ID = newMemoryID()
				}
				if upd.CreatedAt.IsZero() {
					upd.CreatedAt = now()
				}
				sess.Memory = append(sess.Memory, upd)
				byID[upd.ID] = len(sess.Memory) - 1
			}
		}
	}
	// Add: append.
	for i := range diff.Added {
		m := diff.Added[i]
		if m.ID == "" {
			m.ID = newMemoryID()
		}
		if m.CreatedAt.IsZero() {
			m.CreatedAt = now()
		}
		sess.Memory = append(sess.Memory, m)
	}
}

// cloneSession returns a deep-enough copy of the Session for safe return to
// callers. Maps and slices are copied so callers cannot mutate stored state.
func cloneSession(s *domain.Session) *domain.Session {
	if s == nil {
		return nil
	}
	cp := *s
	if s.Turns != nil {
		cp.Turns = append([]domain.Turn(nil), s.Turns...)
		for i := range cp.Turns {
			if s.Turns[i].ToolCalls != nil {
				cp.Turns[i].ToolCalls = append([]domain.ToolCall(nil), s.Turns[i].ToolCalls...)
			}
		}
	}
	if s.Memory != nil {
		cp.Memory = append([]domain.ExtractedMemory(nil), s.Memory...)
	}
	if s.CommittedAt != nil {
		t := *s.CommittedAt
		cp.CommittedAt = &t
	}
	return &cp
}
