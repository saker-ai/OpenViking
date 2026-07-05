package sandbox

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

func TestNew_ExecDefault(t *testing.T) {
	e, err := New(config.SandboxConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := e.(*ExecExecutor); !ok {
		t.Errorf("New() = %T, want *ExecExecutor", e)
	}
}

// TestNew_ContainerdMissingSocket verifies that New returns a clear,
// actionable error when the containerd socket is not present. The
// error must mention the socket path and the fallback so operators
// know what to do.
func TestNew_ContainerdMissingSocket(t *testing.T) {
	// Point at a socket we know doesn't exist; ensure the parent dir
	// is real (e.g. /tmp) so os.Stat fails on the file, not the dir.
	bogus := "/tmp/vikingbot-no-containerd-xxx.sock"
	_ = os.Remove(bogus)
	_, err := NewContainerd(config.SandboxConfig{Backend: "containerd"}, bogus, "docker.io/busybox:latest")
	if err == nil {
		t.Fatalf("New should error when containerd socket is missing")
	}
	if !strings.Contains(err.Error(), "containerd socket") {
		t.Errorf("err = %v, want 'containerd socket'", err)
	}
	if !strings.Contains(err.Error(), "sandbox=exec") {
		t.Errorf("err = %v, want 'sandbox=exec' hint", err)
	}
}

// TestNew_ContainerdSkipsWhenSocketMissing verifies that the containerd
// executor test path t.Skips when the system containerd socket is not
// available. CI environments without containerd should not fail this
// test.
func TestNew_ContainerdSkipsWhenSocketMissing(t *testing.T) {
	if _, err := os.Stat(defaultContainerdSocket); err != nil {
		t.Skipf("containerd socket %q not available: %v", defaultContainerdSocket, err)
	}
	// Even when the socket exists, the test runner may not have
	// permission to dial it (the socket is root:root 0660 by default).
	// We detect that by trying to open it for read; on permission
	// failure we skip rather than fail.
	if err := checkSocketDialable(defaultContainerdSocket); err != nil {
		t.Skipf("containerd socket not dialable: %v", err)
	}
	e, err := New(config.SandboxConfig{Backend: "containerd"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := e.(*ContainerdExecutor); !ok {
		t.Errorf("New() = %T, want *ContainerdExecutor", e)
	}
	if ce, ok := e.(*ContainerdExecutor); ok {
		_ = ce.Close()
	}
}

// TestContainerdExecutor_RunEcho is an end-to-end integration test that
// runs `echo hello` inside a busybox container. Skipped when the
// containerd socket is not present, not dialable by the test runner,
// or the busybox image cannot be pulled (e.g. CI environments without
// network egress to Docker Hub).
func TestContainerdExecutor_RunEcho(t *testing.T) {
	if _, err := os.Stat(defaultContainerdSocket); err != nil {
		t.Skipf("containerd socket %q not available: %v", defaultContainerdSocket, err)
	}
	if err := checkSocketDialable(defaultContainerdSocket); err != nil {
		t.Skipf("containerd socket not dialable: %v", err)
	}
	e, err := NewContainerd(
		config.SandboxConfig{Backend: "containerd", Timeout: 60},
		defaultContainerdSocket,
		"docker.io/library/busybox:latest",
	)
	if err != nil {
		t.Fatalf("NewContainerd: %v", err)
	}
	defer e.Close()
	out, err := e.Run(context.Background(), "echo", "hello-from-containerd")
	if err != nil {
		// If the failure is an image pull, skip rather than fail —
		// many CI environments cannot reach Docker Hub.
		if strings.Contains(err.Error(), "pull") || strings.Contains(err.Error(), "resolve") {
			t.Skipf("cannot pull image (network-restricted env?): %v", err)
		}
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(out), "hello-from-containerd") {
		t.Errorf("out = %q", out)
	}
}

// TestContainerdExecutor_EmptyCommand verifies that Run returns a clear
// error when the command is empty. This path doesn't need containerd.
func TestContainerdExecutor_EmptyCommand(t *testing.T) {
	e := &ContainerdExecutor{}
	_, err := e.Run(context.Background(), "")
	if err == nil {
		t.Fatalf("Run should error on empty command")
	}
	if !strings.Contains(err.Error(), "empty command") {
		t.Errorf("err = %v", err)
	}
}

// TestNew_UnknownBackend verifies that unknown backends return a
// descriptive error rather than ErrUnsupported.
func TestNew_UnknownBackend(t *testing.T) {
	_, err := New(config.SandboxConfig{Backend: "bogus"})
	if err == nil {
		t.Fatalf("New should error on unknown backend")
	}
	if !strings.Contains(err.Error(), "unknown backend") {
		t.Errorf("err = %v, want 'unknown backend'", err)
	}
}

func TestExecExecutor_RunEcho(t *testing.T) {
	e := NewExec(config.SandboxConfig{WorkDir: "/tmp", Timeout: 5})
	out, err := e.Run(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("out = %q", out)
	}
}

func TestExecExecutor_RunFailsOnMissingCommand(t *testing.T) {
	e := NewExec(config.SandboxConfig{WorkDir: "/tmp", Timeout: 5})
	_, err := e.Run(context.Background(), "this-command-does-not-exist-xyz")
	if err == nil {
		t.Fatalf("Run should error on missing command")
	}
}

func TestExecExecutor_EmptyCommand(t *testing.T) {
	e := NewExec(config.SandboxConfig{})
	_, err := e.Run(context.Background(), "")
	if err == nil {
		t.Fatalf("Run should error on empty command")
	}
}

func TestExecExecutor_TimeoutHonored(t *testing.T) {
	e := NewExec(config.SandboxConfig{WorkDir: "/tmp", Timeout: 1})
	start := time.Now()
	_, err := e.Run(context.Background(), "sleep", "10")
	dur := time.Since(start)
	if err == nil {
		t.Fatalf("Run should error on timeout")
	}
	if dur > 3*time.Second {
		t.Errorf("Run took %v, want <3s (timeout)", dur)
	}
}

// checkSocketDialable returns nil if the test runner can dial the
// unix socket, or an error explaining why not. We use a real net.Dial
// because os.OpenFile on a unix socket returns ENXIO even when the
// socket is fully usable.
func checkSocketDialable(socket string) error {
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
