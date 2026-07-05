package cli

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/search", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"uri":"a","score":0.9,"snippet":"hello world"}]}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(SearchCmd(rt))
	require.NoError(t, Execute(cmd, []string{"hello"}))
	assert.Contains(t, out.String(), "a")
	assert.Contains(t, out.String(), "hello world")
}

func TestSearchCmdNoResults(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(SearchCmd(rt))
	require.NoError(t, Execute(cmd, []string{"nothing"}))
	assert.Contains(t, out.String(), "No results")
}

func TestFindCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/relations", r.URL.Path)
		require.Equal(t, "q=myquery", r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"uri":"a","type":"file","score":0.5}]}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(FindCmd(rt))
	require.NoError(t, Execute(cmd, []string{"myquery"}))
	assert.Contains(t, out.String(), "a")
}

func TestSkillsListCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/skills", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"uri":"viking://skills/x","name":"x","steps":[{"order":1,"action":"read"}]}]`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(SkillsCmd(rt))
	require.NoError(t, Execute(cmd, []string{"list"}))
	assert.Contains(t, out.String(), "viking://skills/x")
}

func TestSessionCreateCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sessions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sess-1","status":"active"}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(SessionCmd(rt))
	require.NoError(t, Execute(cmd, []string{"create", "--title", "Test"}))
	assert.Contains(t, out.String(), "session: sess-1")
}

func TestSessionCommitCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sessions/sess-1/commit", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"memory_diff":{"added":[{"id":"m1"}]}}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(SessionCmd(rt))
	require.NoError(t, Execute(cmd, []string{"commit", "sess-1"}))
	assert.Contains(t, out.String(), "committed session: sess-1")
	assert.Contains(t, out.String(), "memories added: 1")
}

func TestTaskListCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/tasks", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"t1","type":"index","status":"done"}]}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(TaskCmd(rt))
	require.NoError(t, Execute(cmd, []string{"list"}))
	assert.Contains(t, out.String(), "t1")
}

func TestObserverListCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/observer", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"o1","target":"x","event":"write"}]}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(ObserverCmd(rt))
	require.NoError(t, Execute(cmd, []string{"list"}))
	assert.Contains(t, out.String(), "o1")
}

func TestSnapshotCreateCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/snapshot", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"snap-1"}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(SnapshotCmd(rt))
	require.NoError(t, Execute(cmd, []string{"create", "--label", "first"}))
	assert.Contains(t, out.String(), "snapshot: snap-1")
}

func TestSnapshotRestoreCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/snapshot/snap-1/restore", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(SnapshotCmd(rt))
	require.NoError(t, Execute(cmd, []string{"restore", "snap-1"}))
	assert.Contains(t, out.String(), "restored: snap-1")
}

func TestPrivacySetCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/privacy-configs/p1", r.URL.Path)
		require.Equal(t, http.MethodPut, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"p1"}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(PrivacyCmd(rt))
	require.NoError(t, Execute(cmd, []string{"set", "p1", "--level=redact"}))
	assert.Contains(t, out.String(), "p1")
}

func TestPrivacyGetCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/privacy-configs/p1", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"p1","level":"redact"}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(PrivacyCmd(rt))
	require.NoError(t, Execute(cmd, []string{"get", "p1"}))
	assert.Contains(t, out.String(), "redact")
}

func TestAdminAccountListCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/admin/accounts", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"a1","name":"acct"}]}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(AdminCmd(rt))
	require.NoError(t, Execute(cmd, []string{"account", "list"}))
	assert.Contains(t, out.String(), "a1")
}

func TestAdminUserCreateCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/admin/users", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u1"}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(AdminCmd(rt))
	require.NoError(t, Execute(cmd, []string{"user", "create", "alice"}))
	assert.Contains(t, out.String(), "user: u1")
}

func TestAdminKeyCreateCmd(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/admin/accounts/acc-1/api-keys", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_key":"key-123"}`))
	})
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(AdminCmd(rt))
	require.NoError(t, Execute(cmd, []string{"key", "create", "acc-1"}))
	assert.Contains(t, out.String(), "key-123")
}
