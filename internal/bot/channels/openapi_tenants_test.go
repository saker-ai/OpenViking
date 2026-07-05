package channels

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// openAPITenantTestConfig returns a ChannelConfig with the given
// gateway token and a list of tenants wired into cfg.Extra["tenants"].
// Each tenant is a map[string]any so the config-loading path is
// exercised end-to-end. The tenants slice is converted to []any so
// initTenantsFromConfig's raw.([]any) type assertion succeeds (Go does
// not allow asserting []map[string]any directly to []any).
func openAPITenantTestConfig(addr, gateway string, tenants ...map[string]any) map[string]any {
	extra := map[string]any{
		"timeout_seconds": 5,
	}
	if len(tenants) > 0 {
		tenantsAny := make([]any, len(tenants))
		for i, t := range tenants {
			tenantsAny[i] = t
		}
		extra["tenants"] = tenantsAny
	}
	return extra
}

// startTenantTestServer boots an openAPIServer with a gateway token and
// the given tenant configs, returning the base URL + cancel func.
func startTenantTestServer(t *testing.T, gateway string, tenants []map[string]any, handler Handler) (string, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := openAPITestConfig(addr, gateway)
	cfg.Extra = openAPITenantTestConfig(addr, gateway, tenants...)
	srv, err := newOpenAPIServer(cfg)
	if err != nil {
		t.Fatalf("newOpenAPIServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx, handler) }()
	for i := 0; i < 100; i++ {
		c, derr := net.Dial("tcp", addr)
		if derr == nil {
			_ = c.Close()
			return "http://" + addr, cancel
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	cancel()
	t.Fatalf("server did not start in time")
	return "", cancel
}

// TestOpenAPI_TenantRouteRegistered verifies that a tenant declared in
// cfg.Extra["tenants"] is reachable via /tenants/{id}/health and that
// the per-tenant token is enforced.
func TestOpenAPI_TenantRouteRegistered(t *testing.T) {
	tenants := []map[string]any{
		{"id": "alpha", "token": "alpha-secret", "quota_per_min": 0},
		{"id": "beta", "token": "beta-secret", "quota_per_min": 0},
	}
	url, cancel := startTenantTestServer(t, "", tenants, func(context.Context, IncomingMessage) error { return nil })
	defer cancel()

	// alpha with correct token: 200.
	req, _ := http.NewRequest("GET", url+"/tenants/alpha/health", nil)
	req.Header.Set("X-Tenant-Token", "alpha-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("alpha health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("alpha health status=%d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// alpha with wrong token: 403.
	req, _ = http.NewRequest("GET", url+"/tenants/alpha/health", nil)
	req.Header.Set("X-Tenant-Token", "wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("alpha wrong token: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("alpha wrong token status=%d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown tenant: 404.
	req, _ = http.NewRequest("GET", url+"/tenants/unknown/health", nil)
	req.Header.Set("X-Tenant-Token", "anything")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unknown tenant: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown tenant status=%d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestOpenAPI_TenantSessionIsolation verifies that two tenants using the
// same session_id do not collide — each gets its own session record.
func TestOpenAPI_TenantSessionIsolation(t *testing.T) {
	tenants := []map[string]any{
		{"id": "alpha", "token": "a", "quota_per_min": 0},
		{"id": "beta", "token": "b", "quota_per_min": 0},
	}
	url, cancel := startTenantTestServer(t, "", tenants, func(context.Context, IncomingMessage) error { return nil })
	defer cancel()

	// Create session "shared" under tenant alpha.
	body := strings.NewReader(`{"user_id":"alice"}`)
	req, _ := http.NewRequest("POST", url+"/tenants/alpha/sessions", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Token", "a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("alpha create session: %v", err)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	alphaSID, _ := created["session_id"].(string)
	if !strings.HasPrefix(alphaSID, "alpha:") {
		t.Errorf("alpha session_id=%q, want 'alpha:'-prefixed", alphaSID)
	}

	// Create session "shared" under tenant beta — should produce a
	// different scoped ID.
	body2 := strings.NewReader(`{"user_id":"bob"}`)
	req, _ = http.NewRequest("POST", url+"/tenants/beta/sessions", body2)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Token", "b")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("beta create session: %v", err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	betaSID, _ := created["session_id"].(string)
	if !strings.HasPrefix(betaSID, "beta:") {
		t.Errorf("beta session_id=%q, want 'beta:'-prefixed", betaSID)
	}
	if betaSID == alphaSID {
		t.Errorf("tenant session IDs collided: %q", alphaSID)
	}

	// List sessions under alpha — should only see alpha's.
	req, _ = http.NewRequest("GET", url+"/tenants/alpha/sessions", nil)
	req.Header.Set("X-Tenant-Token", "a")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("alpha list sessions: %v", err)
	}
	var list sessionListResponse
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if list.Total != 1 {
		t.Errorf("alpha session list total=%d, want 1", list.Total)
	}
	if len(list.Sessions) != 1 || !strings.HasPrefix(list.Sessions[0].ID, "alpha:") {
		t.Errorf("alpha session list returned wrong entries: %+v", list.Sessions)
	}
}

// TestOpenAPI_TenantQuotaEnforced verifies that the per-tenant
// quota_per_min limit rejects requests once exceeded.
func TestOpenAPI_TenantQuotaEnforced(t *testing.T) {
	tenants := []map[string]any{
		{"id": "quota", "token": "q", "quota_per_min": 2},
	}
	url, cancel := startTenantTestServer(t, "", tenants, func(context.Context, IncomingMessage) error { return nil })
	defer cancel()

	// First two requests: 200.
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", url+"/tenants/quota/health", nil)
		req.Header.Set("X-Tenant-Token", "q")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("quota request %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("quota request %d status=%d, want 200", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Third request: 429.
	req, _ := http.NewRequest("GET", url+"/tenants/quota/health", nil)
	req.Header.Set("X-Tenant-Token", "q")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("quota over-limit request: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("quota over-limit status=%d, want 429", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestOpenAPI_TenantUnknownSubroute verifies that unknown subroutes
// under /tenants/{id}/ return 404.
func TestOpenAPI_TenantUnknownSubroute(t *testing.T) {
	tenants := []map[string]any{{"id": "alpha", "token": "a", "quota_per_min": 0}}
	url, cancel := startTenantTestServer(t, "", tenants, func(context.Context, IncomingMessage) error { return nil })
	defer cancel()

	req, _ := http.NewRequest("GET", url+"/tenants/alpha/unknown", nil)
	req.Header.Set("X-Tenant-Token", "a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unknown subroute: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown subroute status=%d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestOpenAPI_TenantRegisterProgrammatic verifies the programmatic API
// RegisterTenant / LookupTenant / UnregisterTenant methods.
func TestOpenAPI_TenantRegisterProgrammatic(t *testing.T) {
	cfg := openAPITestConfig("127.0.0.1:0", "")
	srv, err := newOpenAPIServer(cfg)
	if err != nil {
		t.Fatalf("newOpenAPIServer: %v", err)
	}
	if srv.LookupTenant("p") != nil {
		t.Fatalf("LookupTenant before register should return nil")
	}
	prev := srv.RegisterTenant(TenantConfig{ID: "p", Token: "tok", QuotaPerMin: 0})
	if prev != nil {
		t.Errorf("first Register returned prev=%v, want nil", prev)
	}
	got := srv.LookupTenant("p")
	if got == nil || got.Token != "tok" {
		t.Errorf("LookupTenant after register=%+v, want token 'tok'", got)
	}
	prev = srv.RegisterTenant(TenantConfig{ID: "p", Token: "new", QuotaPerMin: 0})
	if prev == nil || prev.Token != "tok" {
		t.Errorf("re-register returned prev=%+v, want old token 'tok'", prev)
	}
	removed := srv.UnregisterTenant("p")
	if removed == nil || removed.Token != "new" {
		t.Errorf("Unregister returned=%+v, want token 'new'", removed)
	}
	if srv.LookupTenant("p") != nil {
		t.Errorf("LookupTenant after unregister should return nil")
	}
}

// TestOpenAPI_TenantScopeHelper verifies the tenantScope helper directly.
func TestOpenAPI_TenantScopeHelper(t *testing.T) {
	cases := []struct {
		tenant, session, want string
	}{
		{"", "abc", "abc"},
		{"t1", "abc", "t1:abc"},
		{"t1", "", "t1:"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := tenantScope(c.tenant, c.session); got != c.want {
			t.Errorf("tenantScope(%q,%q)=%q, want %q", c.tenant, c.session, got, c.want)
		}
	}
}

// TestOpenAPI_TenantInitFromConfig_MalformedSilent verifies that
// malformed tenant entries are skipped silently so the server still
// boots.
func TestOpenAPI_TenantInitFromConfig_MalformedSilent(t *testing.T) {
	cfg := openAPITestConfig("127.0.0.1:0", "")
	cfg.Extra = map[string]any{
		"tenants": []any{
			map[string]any{"id": "good", "token": "g", "quota_per_min": 0},
			map[string]any{"id": "", "token": "missing id"},      // skipped: empty id
			map[string]any{"token": "no id"},                     // skipped: no id
			"not a map",                                          // skipped: wrong type
			map[string]any{"id": "no-token"},                     // OK: no token (gateway auth only)
		},
	}
	srv, err := newOpenAPIServer(cfg)
	if err != nil {
		t.Fatalf("newOpenAPIServer: %v", err)
	}
	if srv.LookupTenant("good") == nil {
		t.Errorf("tenant 'good' should be registered")
	}
	if srv.LookupTenant("no-token") == nil {
		t.Errorf("tenant 'no-token' should be registered")
	}
	if srv.LookupTenant("") != nil {
		t.Errorf("empty-id tenant should not be registered")
	}
}
