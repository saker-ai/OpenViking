// Package sandbox is the command executor for vikingbot. The agent
// loop uses it to run shell commands (typically MCP-side tools that
// require local execution) under a configurable backend.
//
// Two backends are supported:
//   - "exec" (default): os/exec with a working directory and timeout.
//   - "containerd": short-lived containers via github.com/containerd/v2.
//
// The containerd backend requires a running containerd daemon; when
// the socket is not accessible New returns a clear error describing
// the missing prerequisite so callers can fall back to sandbox=exec.
package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// Executor runs shell commands on behalf of the agent loop.
type Executor interface {
	// Run executes cmd with args in the configured working directory
	// and timeout. Returns combined stdout+stderr on success.
	Run(ctx context.Context, cmd string, args ...string) ([]byte, error)
}

// New returns an Executor for the given config. Backend "exec" returns
// an ExecExecutor; "containerd" returns a ContainerdExecutor (which
// requires the containerd socket to be accessible); "srt" returns an
// SrtExecutor (which requires node + the wrapper script); "opensandbox"
// returns an OpenSandboxExecutor (which requires the opensandbox-server
// to be reachable). Unknown backends return a descriptive error.
//
// The containerd backend reads the socket path from cfg.WorkDir? No —
// from the conventional default /run/containerd/containerd.sock. Pass
// a non-empty socket to NewContainerd to override.
func New(cfg config.SandboxConfig) (Executor, error) {
	switch cfg.Backend {
	case "", "exec":
		return NewExec(cfg), nil
	case "containerd":
		return NewContainerd(cfg, "", "")
	case "srt":
		return NewSrt(cfg)
	case "opensandbox":
		return NewOpenSandbox(cfg)
	default:
		return nil, fmt.Errorf("sandbox: unknown backend %q (want one of exec, containerd, srt, opensandbox)", cfg.Backend)
	}
}

// ExecExecutor runs commands via os/exec.
type ExecExecutor struct {
	workDir string
	timeout time.Duration
}

// NewExec returns an ExecExecutor.
func NewExec(cfg config.SandboxConfig) *ExecExecutor {
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = "/tmp/vikingbot-sandbox"
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &ExecExecutor{workDir: workDir, timeout: timeout}
}

// Run implements Executor.
func (e *ExecExecutor) Run(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	if cmd == "" {
		return nil, fmt.Errorf("sandbox: empty command")
	}
	// Resolve the working directory (it must be absolute so exec.LookPath
	// doesn't get confused by relative paths).
	workDir, err := filepath.Abs(e.workDir)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve workdir: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	c := exec.CommandContext(cctx, cmd, args...)
	c.Dir = workDir
	c.Env = nil // inherit parent env
	out, err := c.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("sandbox: run %s: %w", cmd, err)
	}
	return out, nil
}
