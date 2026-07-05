package routers

import (
	"errors"
	"net/http"
	"path"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/server/identity"
	"github.com/saker-ai/ctxhub/internal/session"
)

// RegisterSessions wires /api/v1/sessions/* — agent session lifecycle.
//
// All endpoints are scoped by the caller's identity (account/user/peer) read
// from the request context. When identity is absent the handlers return 501
// because session.Store requires a non-empty Identifier.
func RegisterSessions(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/sessions")
	r.GET("", listSessions(deps))
	r.POST("", createSession(deps))
	r.GET("/:id", getSession(deps))
	r.POST("/:id/turns", appendSessionTurn(deps))
	r.POST("/:id/commit", commitSession(deps))
	r.POST("/:id/archive", archiveSession(deps))
	r.POST("/:id/compact", compactSession(deps))
	r.GET("/:id/memory", listSessionMemory(deps))
	r.POST("/:id/memory", applySessionMemoryDiff(deps))
	r.POST("/:id/export-skill", exportSessionSkill(deps))
	// SDK-compat aliases. The Go SDK (sdk/go) calls /messages (single),
	// /messages/batch, /context, and DELETE /:id. Map /messages to
	// appendSessionTurn (payload shape is compatible: {role,content});
	// /context to listSessionMemory; /messages/batch is a new handler
	// that loops AppendTurn; DELETE is a new handler that calls
	// Store.Delete. These are registered after the static /:id/* routes
	// so gin's radix tree resolves them as sub-paths of /:id.
	r.POST("/:id/messages", appendSessionTurn(deps))
	r.POST("/:id/messages/batch", batchAppendMessages(deps))
	r.GET("/:id/context", listSessionMemory(deps))
	r.DELETE("/:id", deleteSession(deps))
	// SDK-compat: GET /:id/archives/:archive_id — read an archived session.
	// The Go SDK calls this to fetch a specific archive. The current Store
	// model has no separate archive ID (Archive just marks status=archived),
	// so the archive_id param is ignored and the session itself is returned.
	r.GET("/:id/archives/:archive_id", getSession(deps))
}

// sessionIdentityFromContext reads the caller identity from the request
// context. Sessions are multi-tenant: handlers refuse to operate without an
// account because session.Store keys every entry by Identifier.
func sessionIdentityFromContext(c *gin.Context) (domain.Identifier, bool) {
	id, ok := identity.FromContext(c.Request.Context())
	if !ok || id.Account == "" {
		return domain.Identifier{}, false
	}
	return id, true
}

// listSessions handles GET /sessions — list sessions for the caller ordered
// by CreatedAt descending.
//
// Response shape mirrors openviking/server/routers/sessions.py list_sessions:
// {"status":"ok","result":[<session>,...]}. The result is the array directly
// (not wrapped under "sessions") so the Go SDK's ListSessions — which
// unmarshals result into []any — works without modification.
func listSessions(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		list, err := deps.Sessions.List(c.Request.Context(), id)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(list))
	}
}

// createSessionRequest is the JSON body for POST /sessions.
//
// Field shape mirrors openviking/server/routers/sessions.py CreateSessionRequest:
//   - session_id: optional Python field; when set, the session is created with
//     the caller-supplied id. Mapped to the session's ID field.
//   - memory_policy: optional Python dict; kept as raw JSON (Go does not yet
//     wire a memory-policy validator).
//   - telemetry: optional Python telemetry request; kept as raw JSON.
//
// The legacy Go "peer" field remains accepted as a Go-specific extension so
// existing Go clients keep working. Python fields take precedence.
type createSessionRequest struct {
	// Python-aligned fields (preferred).
	SessionID    string         `json:"session_id,omitempty"`
	MemoryPolicy map[string]any `json:"memory_policy,omitempty"`
	Telemetry    map[string]any `json:"telemetry,omitempty"`
	// Go-specific extension (kept for backward compat).
	Peer string `json:"peer,omitempty"`
}

// createSession handles POST /sessions — allocate a new active session for
// the caller's identity.
//
// Response shape mirrors openviking/server/routers/sessions.py create_session:
// {"status":"ok","result":<session>}. The Go session struct is kept under
// result so existing Go clients see the same fields.
//
// When req.SessionID is set (Python field), the server honors the caller-
// supplied ID via Store.CreateWithID. This is the SDK-compat path: the Go
// SDK posts {"session_id":"..."} and expects subsequent GetSession /
// AddMessage / etc. calls against that ID to resolve. When SessionID is
// empty, falls back to Store.Create (server-generated ID).
func createSession(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		var req createSessionRequest
		// Body is optional; ignore decode errors when the body is empty.
		if c.Request.ContentLength > 0 {
			if err := c.ShouldBindJSON(&req); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
				return
			}
			if req.Peer != "" {
				id.ActorPeer = req.Peer
			}
		}
		var sess *domain.Session
		var err error
		if req.SessionID != "" {
			sess, err = deps.Sessions.CreateWithID(c.Request.Context(), id, req.SessionID)
		} else {
			sess, err = deps.Sessions.Create(c.Request.Context(), id)
		}
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				abortWithError(c, domain.Wrap(domain.CodeConflict, 409, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, okResponse(sess))
	}
}

// sessionIDParam extracts the :id path parameter.
func sessionIDParam(c *gin.Context) string {
	return path.Clean(c.Param("id"))
}

// getSession handles GET /sessions/:id — fetch a session by ID.
//
// Response shape mirrors openviking/server/routers/sessions.py get_session:
// {"status":"ok","result":<session>}. The Go session struct is kept under
// result.
func getSession(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		sess, err := deps.Sessions.Get(c.Request.Context(), id, sid)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// SDK's SessionExists calls GetSession and checks IsCode(err,
				// "NOT_FOUND") with exact match. The session store returns
				// ErrNotFound (RESOURCE_NOT_FOUND) which would not match; map
				// to NOT_FOUND here so the SDK contract holds.
				abortWithError(c, domain.NewAppError(domain.CodeNotFound, 404, "session not found"))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(sess))
	}
}

// appendTurnRequest is the JSON body for POST /sessions/:id/turns.
type appendTurnRequest struct {
	Role      domain.TurnRole   `json:"role"`
	Content   string            `json:"content"`
	ToolCalls []domain.ToolCall `json:"tool_calls,omitempty"`
	Tokens    int               `json:"tokens,omitempty"`
}

// appendSessionTurn handles POST /sessions/:id/turns — append a Turn to an
// active session.
func appendSessionTurn(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		var req appendTurnRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.Role == "" {
			req.Role = domain.TurnRoleUser
		}
		turn := domain.Turn{
			Role:      req.Role,
			Content:   req.Content,
			ToolCalls: req.ToolCalls,
			Tokens:    req.Tokens,
		}
		if err := deps.Sessions.AppendTurn(c.Request.Context(), id, sid, turn); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			if errors.Is(err, domain.ErrConflict) {
				abortWithError(c, domain.Wrap(domain.CodeConflict, 409, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"session_id": sid, "appended": true}))
	}
}

// commitSession handles POST /sessions/:id/commit — transition a session to
// committed status and run the configured compressor (if any).
//
// Response shape mirrors openviking/server/routers/sessions.py commit_session:
// {"status":"ok","result":<session>}. The Go session struct is kept under
// result.
func commitSession(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		sess, err := deps.Sessions.Commit(c.Request.Context(), id, sid)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			if errors.Is(err, domain.ErrConflict) {
				abortWithError(c, domain.Wrap(domain.CodeConflict, 409, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(sess))
	}
}

// archiveSession handles POST /sessions/:id/archive — transition a session
// to archived status (terminal).
func archiveSession(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		if err := deps.Sessions.Archive(c.Request.Context(), id, sid); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"session_id": sid, "archived": true})
	}
}

// listSessionMemory handles GET /sessions/:id/memory — return the session's
// currently stored (or LLM-extracted) memory.
func listSessionMemory(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		mem, err := deps.Sessions.ExtractMemory(c.Request.Context(), id, sid)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"session_id": sid, "memory": mem}))
	}
}

// applySessionMemoryDiff handles POST /sessions/:id/memory — apply an
// additive / mutating diff to the session's stored memory.
func applySessionMemoryDiff(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		var diff domain.MemoryDiff
		if err := c.ShouldBindJSON(&diff); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if err := deps.Sessions.ApplyMemoryDiff(c.Request.Context(), id, sid, diff); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"session_id": sid, "applied": true})
	}
}

// compactSession handles POST /sessions/:id/compact — run the configured
// compressor on a session. This re-invokes Commit so the compressor runs
// against the current turn list; sessions already committed are returned
// unchanged (idempotent).
func compactSession(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		sess, err := deps.Sessions.Commit(c.Request.Context(), id, sid)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			if errors.Is(err, domain.ErrConflict) {
				abortWithError(c, domain.Wrap(domain.CodeConflict, 409, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"session_id": sid, "compacted": true, "session": sess})
	}
}

// exportSessionSkill handles POST /sessions/:id/export-skill — extract
// skill-typed memory from a session and return it as a skill payload. The
// skill shape is a trimmed ExtractedMemory with the session id as the source.
func exportSessionSkill(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		mem, err := deps.Sessions.ExtractMemory(c.Request.Context(), id, sid)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		skills := make([]domain.ExtractedMemory, 0, len(mem))
		for _, m := range mem {
			if m.Type == domain.MemoryTypeSkill {
				skills = append(skills, m)
			}
		}
		c.JSON(http.StatusOK, gin.H{"session_id": sid, "skills": skills})
	}
}

// batchAppendMessages handles POST /sessions/:id/messages/batch — SDK-compat
// alias that appends multiple messages in one call. The Go SDK posts
// {"messages":[{role,content},...]} and expects result to confirm the
// count. Each message is appended via Store.AppendTurn; if any append
// fails the handler returns the error without rolling back prior appends
// (AppendTurn is idempotent on retry).
func batchAppendMessages(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content,omitempty"`
			} `json:"messages"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		appended := 0
		for _, m := range req.Messages {
			role := domain.TurnRole(m.Role)
			if role == "" {
				role = domain.TurnRoleUser
			}
			turn := domain.Turn{Role: role, Content: m.Content}
			if err := deps.Sessions.AppendTurn(c.Request.Context(), id, sid, turn); err != nil {
				if errors.Is(err, domain.ErrNotFound) {
					abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
					return
				}
				if errors.Is(err, domain.ErrConflict) {
					abortWithError(c, domain.Wrap(domain.CodeConflict, 409, err))
					return
				}
				abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
				return
			}
			appended++
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"session_id": sid,
			"appended":   appended,
		}))
	}
}

// deleteSession handles DELETE /sessions/:id — SDK-compat alias that
// removes a session entirely. The Go SDK's DeleteSession calls this
// endpoint and expects the session to be removed (not just archived).
func deleteSession(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.Sessions == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id, ok := sessionIdentityFromContext(c)
		if !ok {
			abortWithError(c, domain.NewAppError(domain.CodeUnauthorized, 401, "missing identity"))
			return
		}
		sid := sessionIDParam(c)
		if sid == "" || sid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "session id is required"))
			return
		}
		if err := deps.Sessions.Delete(c.Request.Context(), id, sid); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"session_id": sid, "deleted": true}))
	}
}

// Compile-time check that session.Store is referenced so the import is not
// dropped when the file is refactored.
var _ session.Store = (*session.MemoryStore)(nil)
