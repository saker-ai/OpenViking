// Package channels — OpenAPI multi-tenant sub-routes (gap 1.3).
//
// This file adds /tenants/{tenant_id}/... routes to the OpenAPI channel so
// a single OpenViking deployment can serve multiple tenants without
// requiring each tenant to deploy a separate instance. The design is
// intentionally minimal:
//
//   - TenantRegistry: in-memory tenant_id -> TenantConfig map, populated
//     programmatically (no persistence; on restart, tenants must be
//     re-registered). A SQLite-backed registry is a future extension.
//   - Per-tenant auth: X-Tenant-Token header, constant-time compared.
//     The gateway-level X-Gateway-Token still applies when configured.
//   - Per-tenant rate limit: simple in-memory request counter, resets
//     every minute. QuotaPerMin=0 means "unlimited".
//   - Tenant-isolated sessions: tenant_id is prefixed onto the session_id
//     so tenant A's "abc" session cannot collide with tenant B's "abc".
//
// Routes mounted (mirrors the global routes):
//
//	GET  /tenants/{id}/health
//	POST /tenants/{id}/chat
//	POST /tenants/{id}/chat/stream
//	GET  /tenants/{id}/sessions
//	GET  /tenants/{id}/sessions/{sid}
//	POST /tenants/{id}/feedback
//
// The single-tenant routes (/chat, /sessions, ...) remain unchanged —
// callers that don't need multi-tenancy pay no overhead.
package channels

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TenantConfig is the per-tenant configuration. QuotaPerMin=0 means
// unlimited. Token is the expected value of the X-Tenant-Token header
// (compared in constant time).
type TenantConfig struct {
	ID          string
	Token       string
	QuotaPerMin int
}

// TenantRegistry holds the registered tenants. The zero value is not
// usable; use NewTenantRegistry.
type TenantRegistry struct {
	mu      sync.RWMutex
	tenants map[string]*TenantConfig
	// counters tracks per-minute request counts for rate limiting. The
	// key is tenant_id; the value is a pointer to a small struct that
	// gets reset when the minute rolls over.
	counters map[string]*tenantCounter
}

// tenantCounter tracks the request count for a single tenant within the
// current minute window. Reset when minuteSinceEpoch changes.
type tenantCounter struct {
	minute   int64
	requests int
}

// NewTenantRegistry returns an empty TenantRegistry.
func NewTenantRegistry() *TenantRegistry {
	return &TenantRegistry{
		tenants:  make(map[string]*TenantConfig),
		counters: make(map[string]*tenantCounter),
	}
}

// Register adds or replaces a tenant. Returns the previous config if any.
func (r *TenantRegistry) Register(cfg TenantConfig) *TenantConfig {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.tenants[cfg.ID]
	r.tenants[cfg.ID] = &cfg
	return prev
}

// Unregister removes a tenant. Returns the removed config if any.
func (r *TenantRegistry) Unregister(id string) *TenantConfig {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.tenants[id]
	delete(r.tenants, id)
	delete(r.counters, id)
	return prev
}

// Get returns the tenant config for id, or nil when unregistered.
func (r *TenantRegistry) Get(id string) *TenantConfig {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tenants[id]
}

// allowQuota increments the per-minute counter for id and reports whether
// the tenant is still under quota. QuotaPerMin=0 means unlimited.
func (r *TenantRegistry) allowQuota(id string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := r.tenants[id]
	if cfg != nil && cfg.QuotaPerMin == 0 {
		return true
	}
	now := time.Now().Unix() / 60
	c, ok := r.counters[id]
	if !ok || c.minute != now {
		c = &tenantCounter{minute: now, requests: 0}
		r.counters[id] = c
	}
	c.requests++
	if cfg != nil && c.requests > cfg.QuotaPerMin {
		return false
	}
	return true
}

// tenantScope returns "tenant_id:session_id" so sessions are isolated
// across tenants. Callers should use this when constructing the ChatID
// passed to the agent.
func tenantScope(tenantID, sessionID string) string {
	if tenantID == "" {
		return sessionID
	}
	if sessionID == "" {
		return tenantID + ":"
	}
	return tenantID + ":" + sessionID
}

// handleTenantRoute dispatches /tenants/{id}/... requests. The path
// shape is /tenants/{id}/{subroute}. Unknown subroutes return 404.
//
// The tenant must be registered; the X-Tenant-Token header must match
// (constant-time); and the per-minute quota must not be exceeded. When
// all checks pass, the request is dispatched to the same handler used
// by the global /chat, /sessions, ... routes — only the path and the
// session scoping differ.
func (s *openAPIServer) handleTenantRoute(handler Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Strip the /tenants/ prefix and split into [id, rest].
		rest := strings.TrimPrefix(r.URL.Path, "/tenants/")
		if rest == "" {
			writeJSONError(w, http.StatusBadRequest, "tenant id required")
			return
		}
		idx := strings.IndexByte(rest, '/')
		if idx < 0 {
			writeJSONError(w, http.StatusBadRequest, "subroute required")
			return
		}
		tenantID := rest[:idx]
		sub := rest[idx+1:] // e.g. "chat", "sessions/abc"
		if tenantID == "" || sub == "" {
			writeJSONError(w, http.StatusBadRequest, "tenant id and subroute required")
			return
		}
		cfg := s.tenants.Get(tenantID)
		if cfg == nil {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("tenant %q not registered", tenantID))
			return
		}
		// Per-tenant token check (constant-time). Skip when no token is
		// configured for the tenant (assumes upstream gateway auth).
		if cfg.Token != "" {
			got := r.Header.Get("X-Tenant-Token")
			if got == "" {
				writeJSONError(w, http.StatusUnauthorized, "X-Tenant-Token header required")
				return
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Token)) != 1 {
				writeJSONError(w, http.StatusForbidden, "Invalid tenant token")
				return
			}
		}
		if !s.tenants.allowQuota(tenantID) {
			writeJSONError(w, http.StatusTooManyRequests, "tenant quota exceeded")
			return
		}
		// Rewrite the path so the existing handlers see /<sub> and
		// dispatch normally. The session_id in the request body is
		// tenant-scoped in the handlers below.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + sub
		s.dispatchTenant(tenantID, sub, w, r2, handler)
	}
}

// dispatchTenant routes the per-tenant subroute to the appropriate
// handler. The tenantID is used to scope session_id at the ChatID layer.
// Existing handlers accept a tenantID parameter (empty for global routes)
// and internally scope the session_id, so the tenant route just forwards.
func (s *openAPIServer) dispatchTenant(tenantID, sub string, w http.ResponseWriter, r *http.Request, handler Handler) {
	switch sub {
	case "health":
		s.handleHealth(w, r)
	case "chat":
		s.handleChat(w, r, handler, tenantID)
	case "chat/stream":
		s.handleChatStream(w, r, handler, tenantID)
	case "sessions":
		s.handleSessions(w, r, tenantID)
	case "feedback":
		s.handleFeedback(w, r, handler, tenantID)
	default:
		if strings.HasPrefix(sub, "sessions/") {
			// /tenants/{id}/sessions/{sid} — set the path so the existing
			// handleSessionByPath can extract the sid, then forward with
			// the tenantID for scoping.
			r.URL.Path = "/sessions/" + strings.TrimPrefix(sub, "sessions/")
			s.handleSessionByPath(w, r, tenantID)
			return
		}
		writeJSONError(w, http.StatusNotFound, "unknown tenant subroute: "+sub)
	}
}

// initTenantsFromConfig populates the tenant registry from cfg.Extra
// ["tenants"], which is expected to be a list of maps with string
// fields id, token, quota_per_min. Unknown or malformed entries are
// skipped silently — operator config validation should catch these
// upstream, and a single bad tenant should not break the whole server.
//
// Example YAML:
//
//	channels:
//	  - provider: openapi
//	    extra:
//	      tenants:
//	        - id: tenant-a
//	          token: ta-secret
//	          quota_per_min: 60
//	        - id: tenant-b
//	          token: tb-secret
//	          quota_per_min: 120
func (s *openAPIServer) initTenantsFromConfig() {
	if s == nil || s.tenants == nil {
		return
	}
	raw, ok := s.cfg.Extra["tenants"]
	if !ok || raw == nil {
		return
	}
	list, ok := raw.([]any)
	if !ok {
		return
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		token, _ := m["token"].(string)
		quota := 0
		switch v := m["quota_per_min"].(type) {
		case int:
			quota = v
		case int64:
			quota = int(v)
		case float64:
			quota = int(v)
		}
		s.tenants.Register(TenantConfig{
			ID:          id,
			Token:       token,
			QuotaPerMin: quota,
		})
	}
}

// RegisterTenant is the programmatic API for adding a tenant at runtime.
// Returns the previous config if any. Useful for tests and for callers
// that maintain their own tenant store (e.g. a database-backed registry
// that calls RegisterTenant on load).
func (s *openAPIServer) RegisterTenant(cfg TenantConfig) *TenantConfig {
	if s == nil || s.tenants == nil {
		return nil
	}
	return s.tenants.Register(cfg)
}

// UnregisterTenant removes a tenant at runtime.
func (s *openAPIServer) UnregisterTenant(id string) *TenantConfig {
	if s == nil || s.tenants == nil {
		return nil
	}
	return s.tenants.Unregister(id)
}

// LookupTenant returns the tenant config for id, or nil when unknown.
// Primarily used by tests; production callers should go through the
// /tenants/{id}/... HTTP routes.
func (s *openAPIServer) LookupTenant(id string) *TenantConfig {
	if s == nil || s.tenants == nil {
		return nil
	}
	return s.tenants.Get(id)
}
