// Package sandbox — OpenSandbox backend.
//
// This file implements the "opensandbox" backend for the Executor
// interface using the HTTP API exposed by opensandbox-server. The
// server must already be running at cfg.OpenSandbox.ServerURL; the
// backend creates a sandbox per Executor instance (POST /sandboxes),
// runs commands against it (POST /sandboxes/{id}/commands), and tears
// it down on Close (DELETE /sandboxes/{id}).
//
// The HTTP surface mirrors the Python opensandbox SDK's call patterns
// (Sandbox.create / sandbox.commands.run / sandbox.kill). When the
// server is unreachable, New returns a clear error describing the
// missing prerequisite so callers can fall back to sandbox=exec.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// openSandboxHTTPTimeout is the per-request HTTP timeout for the
// opensandbox-server. The server itself enforces sandbox wall-clock
// limits; this is just the client-side bound.
const openSandboxHTTPTimeout = 30 * time.Second

// OpenSandboxExecutor runs commands inside an OpenSandbox cloud
// sandbox via the opensandbox-server HTTP API. It is safe for
// concurrent use: a mutex guards the request/response cycle. The
// sandbox ID is allocated on New and released on Close.
type OpenSandboxExecutor struct {
	cfg        config.SandboxConfig
	baseURL    string
	apiKey     string
	image      string
	timeout    time.Duration
	httpClient *http.Client

	mu        sync.Mutex
	sandboxID string
	started   bool
}

// NewOpenSandbox constructs an OpenSandbox-backed Executor. The server
// URL, image, and timeout come from cfg.OpenSandbox. The sandbox is
// created lazily on the first Run call — New only validates
// configuration and pings the server's /health endpoint to surface a
// clear error before any command is dispatched.
func NewOpenSandbox(cfg config.SandboxConfig) (*OpenSandboxExecutor, error) {
	if cfg.OpenSandbox.ServerURL == "" {
		return nil, fmt.Errorf("sandbox: opensandbox.server_url is required (e.g. http://opensandbox-server:8080)")
	}
	if cfg.OpenSandbox.DefaultImage == "" {
		return nil, fmt.Errorf("sandbox: opensandbox.default_image is required (e.g. docker.io/library/busybox:latest)")
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = 30 * time.Second
	}
	rt := cfg.OpenSandbox.RuntimeTimeout
	if rt <= 0 {
		rt = 300
	}
	e := &OpenSandboxExecutor{
		cfg:     cfg,
		baseURL: strings.TrimRight(cfg.OpenSandbox.ServerURL, "/"),
		apiKey:  cfg.OpenSandbox.APIKey,
		image:   cfg.OpenSandbox.DefaultImage,
		timeout: timeout,
		httpClient: &http.Client{
			Timeout: openSandboxHTTPTimeout,
		},
	}
	// Probe /health so a misconfigured server URL fails fast at New
	// rather than on the first Run. Same fail-fast contract as the
	// containerd backend's socket check.
	if err := e.probeHealth(context.Background(), rt); err != nil {
		return nil, fmt.Errorf("sandbox: opensandbox server %q not ready: %w", e.baseURL, err)
	}
	return e, nil
}

// Run implements Executor. The first call lazily creates the sandbox
// (POST /sandboxes); subsequent calls reuse it. ctx cancellation
// propagates to the HTTP request.
func (e *OpenSandboxExecutor) Run(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	if cmd == "" {
		return nil, fmt.Errorf("sandbox: empty command")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	full := strings.Join(append([]string{cmd}, args...), " ")
	// Fast path for pwd — return the canonical sandbox workspace path
	// without creating a sandbox. Avoids allocate-then-immediately-
	// destroy churn for a trivial query. Matches the Python
	// OpenSandbox backend's pwd fast path.
	if strings.TrimSpace(full) == "pwd" {
		return []byte("/workspace"), nil
	}
	if err := e.ensureStarted(ctx); err != nil {
		return nil, err
	}
	body := map[string]any{
		"command": full,
		"timeout": int(e.timeout / time.Second),
	}
	resp, err := e.doJSON(ctx, http.MethodPost, "/sandboxes/"+e.sandboxID+"/commands", body)
	if err != nil {
		return nil, fmt.Errorf("sandbox: opensandbox run: %w", err)
	}
	defer io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sandbox: opensandbox run: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("sandbox: opensandbox decode: %w", err)
	}
	var parts []string
	if result.Stdout != "" {
		parts = append(parts, result.Stdout)
	}
	if result.Stderr != "" {
		parts = append(parts, "STDERR:\n"+result.Stderr)
	}
	if result.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("\nExit code: %d", result.ExitCode))
	}
	if len(parts) == 0 {
		return []byte("(no output)"), nil
	}
	return []byte(strings.Join(parts, "\n")), nil
}

// Close releases the sandbox. It sends a DELETE to /sandboxes/{id} so
// the server reclaims the underlying resources. Safe to call multiple
// times.
func (e *OpenSandboxExecutor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sandboxID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), openSandboxHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, e.baseURL+"/sandboxes/"+e.sandboxID, nil)
	if err != nil {
		return fmt.Errorf("sandbox: opensandbox delete request: %w", err)
	}
	e.attachAuth(req)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: opensandbox delete: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	e.sandboxID = ""
	e.started = false
	return nil
}

// ensureStarted creates the sandbox if it hasn't been created yet.
// Callers must hold e.mu.
func (e *OpenSandboxExecutor) ensureStarted(ctx context.Context) error {
	if e.started {
		return nil
	}
	rt := e.cfg.OpenSandbox.RuntimeTimeout
	if rt <= 0 {
		rt = 300
	}
	body := map[string]any{
		"image":   e.image,
		"timeout": rt,
	}
	resp, err := e.doJSON(ctx, http.MethodPost, "/sandboxes", body)
	if err != nil {
		return fmt.Errorf("sandbox: opensandbox create: %w", err)
	}
	defer io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sandbox: opensandbox create: HTTP %d", resp.StatusCode)
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("sandbox: opensandbox decode create: %w", err)
	}
	if result.ID == "" {
		return errors.New("sandbox: opensandbox create: server returned empty id")
	}
	e.sandboxID = result.ID
	e.started = true
	return nil
}

// probeHealth pings /health once to verify the server is reachable.
// Returns an error describing what went wrong (DNS, connect, status).
func (e *OpenSandboxExecutor) probeHealth(ctx context.Context, _ int) error {
	ctx, cancel := context.WithTimeout(ctx, openSandboxHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	e.attachAuth(req)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// doJSON builds an authenticated JSON request, sends it, and returns
// the raw response. The caller is responsible for closing the body.
func (e *OpenSandboxExecutor) doJSON(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	e.attachAuth(req)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// attachAuth sets the Authorization header when an API key is
// configured. No-op when apiKey is empty.
func (e *OpenSandboxExecutor) attachAuth(req *http.Request) {
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
}

// Compile-time assertion that OpenSandboxExecutor satisfies Executor.
var _ Executor = (*OpenSandboxExecutor)(nil)
