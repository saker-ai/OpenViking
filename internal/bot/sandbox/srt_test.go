package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// TestNewSrt_MissingWrapperPath verifies NewSrt returns a clear error
// when the wrapper path is not configured.
func TestNewSrt_MissingWrapperPath(t *testing.T) {
	_, err := NewSrt(config.SandboxConfig{Backend: "srt"})
	if err == nil {
		t.Fatalf("NewSrt should error when wrapper_path is missing")
	}
	if !strings.Contains(err.Error(), "wrapper_path") {
		t.Errorf("err = %v, want 'wrapper_path'", err)
	}
}

// TestNewSrt_WrapperPathNotAccessible verifies NewSrt errors when the
// wrapper script file does not exist.
func TestNewSrt_WrapperPathNotAccessible(t *testing.T) {
	_, err := NewSrt(config.SandboxConfig{
		Backend: "srt",
		SRT:     config.SRTConfig{WrapperPath: "/nonexistent/srt-wrapper.mjs"},
	})
	if err == nil {
		t.Fatalf("NewSrt should error when wrapper path does not exist")
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("err = %v, want 'not accessible'", err)
	}
}

// TestNewSrt_NodeNotInPath verifies NewSrt errors when the node binary
// is not found in PATH.
func TestNewSrt_NodeNotInPath(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "srt-wrapper.mjs")
	if err := os.WriteFile(wrapper, []byte("// stub"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := NewSrt(config.SandboxConfig{
		Backend: "srt",
		SRT: config.SRTConfig{
			WrapperPath: wrapper,
			NodePath:    "/nonexistent/node-binary-xyz",
		},
	})
	if err == nil {
		t.Fatalf("NewSrt should error when node is not in PATH")
	}
	if !strings.Contains(err.Error(), "node_path") && !strings.Contains(err.Error(), "PATH") {
		t.Errorf("err = %v, want 'node_path' or 'PATH'", err)
	}
}

// TestNewSrt_DefaultsApplied verifies NewSrt fills in default node
// path and timeout when not set.
func TestNewSrt_DefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "srt-wrapper.mjs")
	if err := os.WriteFile(wrapper, []byte("// stub"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not in PATH: %v", err)
	}
	e, err := NewSrt(config.SandboxConfig{
		Backend: "srt",
		SRT:     config.SRTConfig{WrapperPath: wrapper},
	})
	if err != nil {
		t.Fatalf("NewSrt: %v", err)
	}
	if e.nodePath != "node" {
		t.Errorf("nodePath = %q, want 'node'", e.nodePath)
	}
	if e.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", e.timeout)
	}
	if e.workDir != "/tmp/vikingbot-sandbox" {
		t.Errorf("workDir = %q, want '/tmp/vikingbot-sandbox'", e.workDir)
	}
}

// TestSrtConfig_PolicyShape verifies the settings dict has the network
// and filesystem sections the wrapper expects, and that the workspace
// + /tmp are always in allowWrite.
func TestSrtConfig_PolicyShape(t *testing.T) {
	e := &SrtExecutor{
		workDir: "/tmp/ws",
		cfg: config.SandboxConfig{
			WorkDir: "/tmp/ws",
			SRT: config.SRTConfig{
				AllowedDomains:    []string{"example.com"},
				DeniedDomains:     []string{"evil.example"},
				AllowLocalBinding: true,
				DenyRead:          []string{"/etc/shadow"},
				DenyWrite:         []string{"/etc"},
			},
		},
	}
	got := e.srtConfig()
	net, _ := got["network"].(map[string]any)
	if net == nil {
		t.Fatalf("network section missing")
	}
	if net["allowedDomains"] == nil {
		t.Errorf("allowedDomains missing")
	}
	if net["allowLocalBinding"] != true {
		t.Errorf("allowLocalBinding = %v, want true", net["allowLocalBinding"])
	}
	fs, _ := got["filesystem"].(map[string]any)
	if fs == nil {
		t.Fatalf("filesystem section missing")
	}
	allowWrite, _ := fs["allowWrite"].([]string)
	want := []string{"/tmp/ws", "/tmp"}
	if len(allowWrite) != 2 || allowWrite[0] != want[0] || allowWrite[1] != want[1] {
		t.Errorf("allowWrite = %v, want %v", allowWrite, want)
	}
}

// TestSrtFormatExecuted_Table verifies the response formatter across
// stdout/stderr/exit-code combinations.
func TestSrtFormatExecuted_Table(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want string
	}{
		{
			name: "stdout only",
			resp: map[string]any{"stdout": "hello", "stderr": "", "exitCode": float64(0)},
			want: "hello",
		},
		{
			name: "stderr only",
			resp: map[string]any{"stdout": "", "stderr": "warn", "exitCode": float64(0)},
			want: "STDERR:\nwarn",
		},
		{
			name: "nonzero exit",
			resp: map[string]any{"stdout": "out", "stderr": "err", "exitCode": float64(2)},
			want: "out\nSTDERR:\nerr\n\nExit code: 2",
		},
		{
			name: "no output",
			resp: map[string]any{"stdout": "", "stderr": "", "exitCode": float64(0)},
			want: "(no output)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := srtFormatExecuted(tc.resp)
			if got != tc.want {
				t.Errorf("got = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSrtExecutor_PwdFastPath verifies the pwd command is handled
// locally without round-tripping to the wrapper. This matches the
// Python SRT backend's pwd fast path.
func TestSrtExecutor_PwdFastPath(t *testing.T) {
	e := &SrtExecutor{workDir: "/tmp/ws"}
	out, err := e.Run(context.Background(), "pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "/tmp/ws" {
		t.Errorf("out = %q, want '/tmp/ws'", out)
	}
}

// TestSrtExecutor_EmptyCommand verifies Run rejects empty commands
// without spawning the wrapper.
func TestSrtExecutor_EmptyCommand(t *testing.T) {
	e := &SrtExecutor{}
	_, err := e.Run(context.Background(), "")
	if err == nil {
		t.Fatalf("Run should error on empty command")
	}
	if !strings.Contains(err.Error(), "empty command") {
		t.Errorf("err = %v, want 'empty command'", err)
	}
}

// TestSrtExecutor_WriteSettings verifies writeSettings creates the
// sandboxes/ directory under the workspace and writes valid JSON.
func TestSrtExecutor_WriteSettings(t *testing.T) {
	dir := t.TempDir()
	e := &SrtExecutor{
		cfg: config.SandboxConfig{
			WorkDir: dir,
			SRT:     config.SRTConfig{AllowedDomains: []string{"example.com"}},
		},
		workDir: dir,
	}
	path, err := e.writeSettings()
	if err != nil {
		t.Fatalf("writeSettings: %v", err)
	}
	if !strings.HasPrefix(path, filepath.Join(dir, "sandboxes")) {
		t.Errorf("path = %q, want under sandboxes/", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["network"] == nil {
		t.Errorf("network section missing from settings")
	}
}

// TestSrtExecutor_EndToEndWithFakeWrapper runs the full SRT pipeline
// against a fake "node" shell script that speaks the JSON-line
// protocol. Verifies start, execute, and stop all work.
func TestSrtExecutor_EndToEndWithFakeWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake wrapper not portable to windows")
	}
	dir := t.TempDir()
	fakeNode := filepath.Join(dir, "fake-node.sh")
	wrapperPath := filepath.Join(dir, "srt-wrapper.mjs")
	// The fake node script takes argv[1]=wrapperPath argv[2]=settings
	// argv[3]=workspace, then speaks the JSON-line protocol.
	script := `#!/bin/sh
printf '{"type":"ready"}\n'
first=1
while IFS= read -r line; do
	if [ "$first" = "1" ]; then
		printf '{"type":"initialized","warnings":[]}\n'
		first=0
		continue
	fi
	case "$line" in
		*'"type":"execute"'*)
			printf '{"type":"executed","stdout":"hello from fake wrapper","stderr":"","exitCode":0,"violations":[]}\n'
			;;
		*'"type":"reset"'*)
			exit 0
			;;
		*)
			printf '{"type":"error","message":"unknown message"}\n'
			exit 1
			;;
	esac
done
`
	if err := os.WriteFile(fakeNode, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake-node: %v", err)
	}
	if err := os.WriteFile(wrapperPath, []byte("// stub"), 0o644); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	e, err := NewSrt(config.SandboxConfig{
		Backend: "srt",
		WorkDir: filepath.Join(dir, "ws"),
		Timeout: 10,
		SRT:     config.SRTConfig{WrapperPath: wrapperPath, NodePath: fakeNode},
	})
	if err != nil {
		t.Fatalf("NewSrt: %v", err)
	}
	defer e.Close()
	out, err := e.Run(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(out), "hello from fake wrapper") {
		t.Errorf("out = %q, want 'hello from fake wrapper'", out)
	}
}

// TestSrtExecutor_InitializeFailed verifies that when the wrapper
// responds with initialize_failed, Run returns a descriptive error.
func TestSrtExecutor_InitializeFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake wrapper not portable to windows")
	}
	dir := t.TempDir()
	fakeNode := filepath.Join(dir, "fake-node.sh")
	wrapperPath := filepath.Join(dir, "srt-wrapper.mjs")
	script := `#!/bin/sh
printf '{"type":"ready"}\n'
read line
printf '{"type":"initialize_failed","errors":["bad policy"],"warnings":[]}\n'
`
	if err := os.WriteFile(fakeNode, []byte(script), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(wrapperPath, []byte("// stub"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	e, err := NewSrt(config.SandboxConfig{
		Backend: "srt",
		WorkDir: filepath.Join(dir, "ws"),
		Timeout: 5,
		SRT:     config.SRTConfig{WrapperPath: wrapperPath, NodePath: fakeNode},
	})
	if err != nil {
		t.Fatalf("NewSrt: %v", err)
	}
	defer e.Close()
	_, err = e.Run(context.Background(), "echo", "hello")
	if err == nil {
		t.Fatalf("Run should fail when initialize fails")
	}
	if !strings.Contains(err.Error(), "initialize failed") {
		t.Errorf("err = %v, want 'initialize failed'", err)
	}
	if !strings.Contains(err.Error(), "bad policy") {
		t.Errorf("err = %v, want 'bad policy'", err)
	}
}
