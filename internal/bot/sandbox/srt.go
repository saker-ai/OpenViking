// Package sandbox — SRT (sandbox-runtime) backend.
//
// This file implements the "srt" backend for the Executor interface
// using @anthropic-ai/sandbox-runtime via a Node.js wrapper process.
// The wrapper is an ES module (.mjs) that reads a JSON settings file
// from argv[1] and the workspace path from argv[2], then communicates
// with the host via newline-delimited JSON on stdin/stdout.
//
// The protocol mirrors the Python vikingbot sandbox/backends/srt.py:
//
//	host                  wrapper
//	 ---                  ---
//	  <- {"type":"ready"}
//	 -> {"type":"initialize","config":{...}}
//	  <- {"type":"initialized"} or {"type":"initialize_failed","errors":[...]}
//	 -> {"type":"execute","command":"...","timeout":60000,"customConfig":null}
//	  <- {"type":"executed","stdout":"...","stderr":"...","exitCode":0,"violations":[]}
//	 -> {"type":"read_file","path":"..."}
//	  <- {"type":"file_read","content":"..."}
//	 -> {"type":"write_file","path":"...","content":"..."}
//	  <- {"type":"file_written"}
//	 -> {"type":"list_dir","path":"..."}
//	  <- {"type":"dir_listed","items":[{"name":"...","is_dir":false},...]}
//	 -> {"type":"reset"}
//
// The wrapper process is spawned lazily on the first Run call and kept
// alive for the lifetime of the executor. Close terminates the process
// (send reset, then SIGTERM, then SIGKILL on timeout).
package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// srtReadyTimeout is the time we wait for the wrapper to send its
// initial "ready" message after process spawn.
const srtReadyTimeout = 30 * time.Second

// srtInitializeTimeout is the time we wait for the wrapper to confirm
// initialization after we send the "initialize" message.
const srtInitializeTimeout = 30 * time.Second

// srtStopGrace is the time between SIGTERM and SIGKILL when stopping
// the wrapper process.
const srtStopGrace = 5 * time.Second

// SrtExecutor runs commands inside @anthropic-ai/sandbox-runtime via a
// Node.js wrapper process. It is safe for concurrent use: a mutex
// guards the request/response cycle so only one command is in flight at
// a time. The wrapper process is single-threaded by design.
type SrtExecutor struct {
	cfg      config.SandboxConfig
	nodePath string
	wrapper  string
	workDir  string
	timeout  time.Duration

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	dec    *json.Decoder
	started bool
}

// NewSrt constructs an SRT-backed Executor. cfg.SRT.WrapperPath must
// point at the srt-wrapper.mjs script; cfg.SRT.NodePath defaults to
// "node" (resolved via PATH) when empty. The wrapper is not spawned
// until the first Run call — NewSrt only validates configuration and
// resolves the working directory.
func NewSrt(cfg config.SandboxConfig) (*SrtExecutor, error) {
	if cfg.SRT.WrapperPath == "" {
		return nil, fmt.Errorf("sandbox: srt.wrapper_path is required (point at the srt-wrapper.mjs script)")
	}
	if _, err := os.Stat(cfg.SRT.WrapperPath); err != nil {
		return nil, fmt.Errorf("sandbox: srt.wrapper_path %q not accessible: %w", cfg.SRT.WrapperPath, err)
	}
	nodePath := cfg.SRT.NodePath
	if nodePath == "" {
		nodePath = "node"
	}
	if _, err := exec.LookPath(nodePath); err != nil {
		return nil, fmt.Errorf("sandbox: srt.node_path %q not in PATH: %w", nodePath, err)
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = 30 * time.Second
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = "/tmp/vikingbot-sandbox"
	}
	return &SrtExecutor{
		cfg:      cfg,
		nodePath: nodePath,
		wrapper:  cfg.SRT.WrapperPath,
		workDir:  workDir,
		timeout:  timeout,
	}, nil
}

// Run implements Executor. The first call lazily spawns the wrapper
// process and performs the initialize handshake. Subsequent calls
// reuse the long-lived process. ctx cancellation propagates to the
// wrapper via a per-call timeout (the wrapper enforces its own
// timeout, but we bound the host-side wait).
func (e *SrtExecutor) Run(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	if cmd == "" {
		return nil, fmt.Errorf("sandbox: empty command")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	full := strings.Join(append([]string{cmd}, args...), " ")
	// Fast path for pwd — return the workspace path without
	// round-tripping to the wrapper (avoids spawn cost for a trivial
	// query). Matches the Python SRT backend's pwd fast path.
	if strings.TrimSpace(full) == "pwd" {
		return []byte(filepath.Clean(e.workDir)), nil
	}
	if err := e.ensureStarted(ctx); err != nil {
		return nil, err
	}
	timeout := e.timeout
	req := map[string]any{
		"type":        "execute",
		"command":     full,
		"timeout":     int(timeout / time.Millisecond),
		"customConfig": nil,
	}
	if err := e.send(req); err != nil {
		return nil, fmt.Errorf("sandbox: srt send execute: %w", err)
	}
	resp, err := e.recv(ctx, "executed")
	if err != nil {
		return nil, fmt.Errorf("sandbox: srt execute: %w", err)
	}
	out := srtFormatExecuted(resp)
	return []byte(out), nil
}

// Close terminates the wrapper process. It sends a best-effort reset
// message, then SIGTERMs the process, waiting up to srtStopGrace for
// exit before SIGKILL. Safe to call multiple times.
func (e *SrtExecutor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cmd == nil {
		return nil
	}
	// Best-effort reset; ignore errors (process may already be gone).
	_ = e.send(map[string]any{"type": "reset"})
	_ = e.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { _, _ = e.cmd.Process.Wait(); done <- nil }()
	select {
	case <-done:
	case <-time.After(srtStopGrace):
		_ = e.cmd.Process.Kill()
		<-done
	}
	if e.stdin != nil {
		_ = e.stdin.Close()
	}
	e.cmd = nil
	e.stdin = nil
	e.stdout = nil
	e.dec = nil
	e.started = false
	return nil
}

// ensureStarted spawns the wrapper process if it hasn't been started
// yet and performs the ready/initialize handshake. Callers must hold
// e.mu.
func (e *SrtExecutor) ensureStarted(ctx context.Context) error {
	if e.started {
		return nil
	}
	settingsPath, err := e.writeSettings()
	if err != nil {
		return err
	}
	absWorkDir, err := filepath.Abs(e.workDir)
	if err != nil {
		return fmt.Errorf("sandbox: resolve workdir: %w", err)
	}
	if err := os.MkdirAll(absWorkDir, 0o755); err != nil {
		return fmt.Errorf("sandbox: mkdir workdir: %w", err)
	}
	c := exec.CommandContext(ctx, e.nodePath, e.wrapper, settingsPath, absWorkDir)
	c.Dir = absWorkDir
	c.Env = os.Environ()
	stdin, err := c.StdinPipe()
	if err != nil {
		return fmt.Errorf("sandbox: srt stdin pipe: %w", err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return fmt.Errorf("sandbox: srt stdout pipe: %w", err)
	}
	// Inherit stderr so wrapper diagnostics surface in the bot log.
	c.Stderr = os.Stderr
	if err := c.Start(); err != nil {
		return fmt.Errorf("sandbox: srt start node: %w", err)
	}
	e.cmd = c
	e.stdin = stdin
	e.stdout = bufio.NewReader(stdout)
	e.dec = json.NewDecoder(e.stdout)
	// Wait for {"type":"ready"}.
	readyCtx, cancel := context.WithTimeout(ctx, srtReadyTimeout)
	defer cancel()
	if _, err := e.recv(readyCtx, "ready"); err != nil {
		_ = c.Process.Kill()
		return fmt.Errorf("sandbox: srt ready handshake: %w", err)
	}
	// Send initialize.
	if err := e.send(map[string]any{
		"type":   "initialize",
		"config": e.srtConfig(),
	}); err != nil {
		_ = c.Process.Kill()
		return fmt.Errorf("sandbox: srt send initialize: %w", err)
	}
	initCtx, cancelInit := context.WithTimeout(ctx, srtInitializeTimeout)
	defer cancelInit()
	resp, err := e.recv(initCtx, "")
	if err != nil {
		_ = c.Process.Kill()
		return fmt.Errorf("sandbox: srt initialize: %w", err)
	}
	if t, _ := resp["type"].(string); t == "initialize_failed" {
		errs, _ := resp["errors"].([]any)
		_ = c.Process.Kill()
		return fmt.Errorf("sandbox: srt initialize failed: %v", errs)
	}
	if t, _ := resp["type"].(string); t != "initialized" {
		_ = c.Process.Kill()
		return fmt.Errorf("sandbox: srt initialize: unexpected response %q", t)
	}
	e.started = true
	return nil
}

// writeSettings writes the SRT settings JSON to a temp file inside the
// workspace's sandboxes/ directory and returns the path. The file is
// re-written on every executor start so policy changes (e.g. updated
// deny lists) take effect on the next restart.
func (e *SrtExecutor) writeSettings() (string, error) {
	absWorkDir, err := filepath.Abs(e.workDir)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve workdir: %w", err)
	}
	sandboxesDir := filepath.Join(absWorkDir, "sandboxes")
	if err := os.MkdirAll(sandboxesDir, 0o755); err != nil {
		return "", fmt.Errorf("sandbox: mkdir sandboxes: %w", err)
	}
	settingsPath := filepath.Join(sandboxesDir, "srt-settings.json")
	data, err := json.MarshalIndent(e.srtConfig(), "", "  ")
	if err != nil {
		return "", fmt.Errorf("sandbox: marshal srt settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		return "", fmt.Errorf("sandbox: write srt settings: %w", err)
	}
	return settingsPath, nil
}

// srtConfig builds the settings dict passed to the wrapper. The
// workspace directory and /tmp are always added to filesystem.allowWrite.
func (e *SrtExecutor) srtConfig() map[string]any {
	absWorkDir, _ := filepath.Abs(e.workDir)
	allowWrite := []string{absWorkDir, "/tmp"}
	return map[string]any{
		"network": map[string]any{
			"allowedDomains":    e.cfg.SRT.AllowedDomains,
			"deniedDomains":     e.cfg.SRT.DeniedDomains,
			"allowLocalBinding": e.cfg.SRT.AllowLocalBinding,
		},
		"filesystem": map[string]any{
			"denyRead":   e.cfg.SRT.DenyRead,
			"allowWrite": allowWrite,
			"denyWrite":  e.cfg.SRT.DenyWrite,
		},
	}
}

// send marshals msg as JSON, appends a newline, and writes it to the
// wrapper's stdin. Callers must hold e.mu.
func (e *SrtExecutor) send(msg map[string]any) error {
	if e.stdin == nil {
		return errors.New("sandbox: srt stdin not open")
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	if _, err := e.stdin.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// recv reads one JSON object from the wrapper's stdout. expectedType
// is the required "type" field; when empty, any type is accepted
// (callers inspect the response themselves). ctx bounds the wait.
func (e *SrtExecutor) recv(ctx context.Context, expectedType string) (map[string]any, error) {
	if e.dec == nil {
		return nil, errors.New("sandbox: srt stdout not open")
	}
	type result struct {
		msg map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var msg map[string]any
		if err := e.dec.Decode(&msg); err != nil {
			ch <- result{nil, fmt.Errorf("decode: %w", err)}
			return
		}
		ch <- result{msg, nil}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if expectedType != "" {
			if t, _ := r.msg["type"].(string); t != expectedType && t != "error" {
				return nil, fmt.Errorf("unexpected response type %q (want %q)", t, expectedType)
			}
			if t, _ := r.msg["type"].(string); t == "error" {
				msg, _ := r.msg["message"].(string)
				return r.msg, fmt.Errorf("wrapper error: %s", msg)
			}
		}
		return r.msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// srtFormatExecuted turns an "executed" response into the combined
// stdout/stderr/exit-code text that the Executor.Run contract returns.
func srtFormatExecuted(resp map[string]any) string {
	var parts []string
	stdout, _ := resp["stdout"].(string)
	stderr, _ := resp["stderr"].(string)
	exitCode, _ := resp["exitCode"].(float64)
	if stdout != "" {
		parts = append(parts, stdout)
	}
	if stderr != "" {
		parts = append(parts, "STDERR:\n"+stderr)
	}
	if exitCode != 0 {
		parts = append(parts, fmt.Sprintf("\nExit code: %d", int(exitCode)))
	}
	if len(parts) == 0 {
		return "(no output)"
	}
	return strings.Join(parts, "\n")
}

// Compile-time assertion that SrtExecutor satisfies Executor.
var _ Executor = (*SrtExecutor)(nil)
