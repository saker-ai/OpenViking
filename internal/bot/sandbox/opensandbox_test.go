package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// opensandboxStub is an httptest.Server that fakes the opensandbox-server
// HTTP API for tests. It records every request received so tests can
// assert on the call sequence.
type opensandboxStub struct {
	t          *testing.T
	server     *httptest.Server
	mu         opensandboxStubMu
	createReqs []map[string]any
	runReqs    []map[string]any
	deleteReqs int
	healthReqs int
	nextID     int
	// failCreate makes POST /sandboxes return 500.
	failCreate bool
	// failRun makes POST /sandboxes/{id}/commands return 500.
	failRun bool
	// runResponse overrides the default executed response body.
	runResponse map[string]any
}

type opensandboxStubMu struct {
	// noop; sync is via httptest.Server's serial handling.
}

// newOpenSandboxStub starts an httptest server and returns it.
func newOpenSandboxStub(t *testing.T, failCreate, failRun bool, runResponse map[string]any) *opensandboxStub {
	t.Helper()
	s := &opensandboxStub{t: t, failCreate: failCreate, failRun: failRun, runResponse: runResponse}
	s.server = httptest.NewServer(http.HandlerFunc(s.handler))
	return s
}

func (s *opensandboxStub) close() { s.server.Close() }
func (s *opensandboxStub) url() string { return s.server.URL }

func (s *opensandboxStub) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health":
		s.healthReqs++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	case r.URL.Path == "/sandboxes" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.createReqs = append(s.createReqs, body)
		if s.failCreate {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.nextID++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"sbx-` + itoa(s.nextID) + `"}`))
		return
	case strings.HasPrefix(r.URL.Path, "/sandboxes/") && strings.HasSuffix(r.URL.Path, "/commands") && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.runReqs = append(s.runReqs, body)
		if s.failRun {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if s.runResponse != nil {
			data, _ := json.Marshal(s.runResponse)
			_, _ = w.Write(data)
			return
		}
		_, _ = w.Write([]byte(`{"stdout":"hello from opensandbox","stderr":"","exit_code":0}`))
		return
	case strings.HasPrefix(r.URL.Path, "/sandboxes/") && r.Method == http.MethodDelete:
		s.deleteReqs++
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// itoa is a small helper to avoid pulling strconv into the test file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}

// TestNewOpenSandbox_MissingServerURL verifies NewOpenSandbox errors
// when server_url is empty.
func TestNewOpenSandbox_MissingServerURL(t *testing.T) {
	_, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err == nil {
		t.Fatalf("NewOpenSandbox should error when server_url is missing")
	}
	if !strings.Contains(err.Error(), "server_url") {
		t.Errorf("err = %v, want 'server_url'", err)
	}
}

// TestNewOpenSandbox_MissingImage verifies NewOpenSandbox errors when
// default_image is empty.
func TestNewOpenSandbox_MissingImage(t *testing.T) {
	_, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL: "http://localhost:8080",
		},
	})
	if err == nil {
		t.Fatalf("NewOpenSandbox should error when default_image is missing")
	}
	if !strings.Contains(err.Error(), "default_image") {
		t.Errorf("err = %v, want 'default_image'", err)
	}
}

// TestNewOpenSandbox_ServerNotReady verifies NewOpenSandbox errors
// when the /health probe fails (server unreachable).
func TestNewOpenSandbox_ServerNotReady(t *testing.T) {
	// Point at a port nothing is listening on. The kernel will RST
	// the connection, surfacing as a connect error.
	_, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    "http://127.0.0.1:1", // port 1 is reserved, nothing listens
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err == nil {
		t.Fatalf("NewOpenSandbox should error when server is unreachable")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("err = %v, want 'not ready'", err)
	}
}

// TestNewOpenSandbox_HealthProbeOK verifies NewOpenSandbox succeeds
// when the /health probe returns 200.
func TestNewOpenSandbox_HealthProbeOK(t *testing.T) {
	stub := newOpenSandboxStub(t, false, false, nil)
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	if stub.healthReqs != 1 {
		t.Errorf("healthReqs = %d, want 1", stub.healthReqs)
	}
}

// TestOpenSandboxExecutor_RunEndToEnd verifies a Run call creates the
// sandbox, posts the command, and returns the stdout.
func TestOpenSandboxExecutor_RunEndToEnd(t *testing.T) {
	stub := newOpenSandboxStub(t, false, false, nil)
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	out, err := e.Run(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(out), "hello from opensandbox") {
		t.Errorf("out = %q", out)
	}
	if len(stub.createReqs) != 1 {
		t.Errorf("createReqs = %d, want 1", len(stub.createReqs))
	}
	if len(stub.runReqs) != 1 {
		t.Errorf("runReqs = %d, want 1", len(stub.runReqs))
	}
	if cmd, _ := stub.runReqs[0]["command"].(string); cmd != "echo hello" {
		t.Errorf("command = %q, want 'echo hello'", cmd)
	}
}

// TestOpenSandboxExecutor_PwdFastPath verifies pwd is handled locally.
func TestOpenSandboxExecutor_PwdFastPath(t *testing.T) {
	stub := newOpenSandboxStub(t, false, false, nil)
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	out, err := e.Run(context.Background(), "pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "/workspace" {
		t.Errorf("out = %q, want '/workspace'", out)
	}
	if len(stub.createReqs) != 0 {
		t.Errorf("pwd should not create a sandbox; createReqs = %d", len(stub.createReqs))
	}
}

// TestOpenSandboxExecutor_EmptyCommand verifies Run rejects empty cmd.
func TestOpenSandboxExecutor_EmptyCommand(t *testing.T) {
	stub := newOpenSandboxStub(t, false, false, nil)
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	_, err = e.Run(context.Background(), "")
	if err == nil {
		t.Fatalf("Run should reject empty command")
	}
	if !strings.Contains(err.Error(), "empty command") {
		t.Errorf("err = %v", err)
	}
}

// TestOpenSandboxExecutor_CreateFailed verifies Run errors when the
// server returns 500 on sandbox creation.
func TestOpenSandboxExecutor_CreateFailed(t *testing.T) {
	stub := newOpenSandboxStub(t, true, false, nil)
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	_, err = e.Run(context.Background(), "echo", "hello")
	if err == nil {
		t.Fatalf("Run should fail when create returns 500")
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("err = %v, want 'create'", err)
	}
}

// TestOpenSandboxExecutor_RunFailed verifies Run errors when the
// server returns 500 on command execution.
func TestOpenSandboxExecutor_RunFailed(t *testing.T) {
	stub := newOpenSandboxStub(t, false, true, nil)
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	_, err = e.Run(context.Background(), "echo", "hello")
	if err == nil {
		t.Fatalf("Run should fail when execute returns 500")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v, want 'HTTP 500'", err)
	}
}

// TestOpenSandboxExecutor_NonzeroExit verifies Run surfaces the exit
// code when the sandbox command returns nonzero.
func TestOpenSandboxExecutor_NonzeroExit(t *testing.T) {
	stub := newOpenSandboxStub(t, false, false, map[string]any{
		"stdout":    "partial output",
		"stderr":    "boom",
		"exit_code": 42,
	})
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	out, err := e.Run(context.Background(), "false")
	if err != nil {
		t.Fatalf("Run: %v (nonzero exit should not be a Run error)", err)
	}
	if !strings.Contains(string(out), "partial output") {
		t.Errorf("out = %q, want 'partial output'", out)
	}
	if !strings.Contains(string(out), "STDERR:") {
		t.Errorf("out = %q, want 'STDERR:'", out)
	}
	if !strings.Contains(string(out), "Exit code: 42") {
		t.Errorf("out = %q, want 'Exit code: 42'", out)
	}
}

// TestOpenSandboxExecutor_CloseIdempotent verifies Close can be called
// multiple times without error.
func TestOpenSandboxExecutor_CloseIdempotent(t *testing.T) {
	stub := newOpenSandboxStub(t, false, false, nil)
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	// Trigger sandbox creation first.
	if _, err := e.Run(context.Background(), "echo", "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close 2: %v (should be no-op)", err)
	}
	if stub.deleteReqs != 1 {
		t.Errorf("deleteReqs = %d, want 1", stub.deleteReqs)
	}
}

// TestOpenSandboxExecutor_AuthHeaderSent verifies the Authorization
// header is set when APIKey is configured.
func TestOpenSandboxExecutor_AuthHeaderSent(t *testing.T) {
	stub := newOpenSandboxStub(t, false, false, nil)
	defer stub.close()
	// Wrap handler to capture the Authorization header.
	var gotAuth string
	orig := stub.server.Config.Handler
	stub.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		orig.ServeHTTP(w, r)
	})
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			APIKey:       "secret-token-xyz",
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	if _, err := e.Run(context.Background(), "echo", "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotAuth != "Bearer secret-token-xyz" {
		t.Errorf("Authorization = %q, want 'Bearer secret-token-xyz'", gotAuth)
	}
}

// TestOpenSandboxExecutor_DefaultsApplied verifies NewOpenSandbox
// fills in the default runtime timeout (300s) when not set. The
// default is applied in ensureStarted so the create request body
// carries 300 even though cfg.OpenSandbox.RuntimeTimeout is 0.
func TestOpenSandboxExecutor_DefaultsApplied(t *testing.T) {
	stub := newOpenSandboxStub(t, false, false, nil)
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	if _, err := e.Run(context.Background(), "echo", "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stub.createReqs) != 1 {
		t.Fatalf("createReqs = %d, want 1", len(stub.createReqs))
	}
	if to, _ := stub.createReqs[0]["timeout"].(float64); to != 300 {
		t.Errorf("create timeout = %v, want 300", stub.createReqs[0]["timeout"])
	}
}

// TestOpenSandboxExecutor_CtxCanceled verifies ctx cancellation
// propagates to the HTTP request.
func TestOpenSandboxExecutor_CtxCanceled(t *testing.T) {
	stub := newOpenSandboxStub(t, false, false, nil)
	defer stub.close()
	e, err := NewOpenSandbox(config.SandboxConfig{
		Backend: "opensandbox",
		OpenSandbox: config.OpenSandboxConfig{
			ServerURL:    stub.url(),
			DefaultImage: "docker.io/library/busybox:latest",
		},
	})
	if err != nil {
		t.Fatalf("NewOpenSandbox: %v", err)
	}
	defer e.Close()
	// Trigger creation first.
	if _, err := e.Run(context.Background(), "echo", "warmup"); err != nil {
		t.Fatalf("warmup Run: %v", err)
	}
	// Now cancel ctx before Run.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	_, err = e.Run(ctx, "echo", "hello")
	if err == nil {
		t.Fatalf("Run should fail with canceled ctx")
	}
}
