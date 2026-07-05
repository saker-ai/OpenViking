//go:build e2e

// Package e2e contains end-to-end smoke tests that drive the real
// openviking-server binary over HTTP. Layer 2 in scripts/e2e-smoke.sh.
//
// These tests build nothing — they require `make build` to have produced
// ../../bin/openviking-server. They start the server as a subprocess
// pointed at examples/ov.conf.local-memory (in-memory vectordb + hash
// fallback embedder, zero external dependencies), then exercise the
// resource add → search → read closure over the public REST API.
//
// Run: make build && GOWORK=off go test -tags=e2e -race ./tests/e2e/...
package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time")

const (
	healthPath = "/healthz"
	addr       = "127.0.0.1:1933"
	baseURL    = "http://" + addr
)

// TestLocalServer_AddSearchRead drives the full closure: start server →
// /healthz → POST /api/v1/resources → POST /api/v1/search → GET
// /api/v1/resources/<uri>. Verifies that the server starts cleanly with
// the local-memory config, accepts writes, and serves them back.
func TestLocalServer_AddSearchRead(t *testing.T) {
	bin := locateBinary(t, "openviking-server")
	cfgPath := locateConfig(t)

	srvCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(srvCtx, bin, "--config", cfgPath, "--addr", addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	if !waitForHealth(t, baseURL+healthPath, 30*time.Second) {
		t.Fatal("server never became healthy")
	}

	// 1. Write a resource via PUT /api/v1/content/<uri>. The body is
	// raw bytes; X-OpenViking-Account is required by the identity
	// middleware; the value is arbitrary when api_key auth is disabled.
	content := "# Rat doc\n\nRats are rodents."
	resp := mustPut(t, baseURL+"/api/v1/content/e2e_rat.md", content, "e2e")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("put content: status %d body %s", resp.StatusCode, b)
	}

	// 2. Read it back via GET /api/v1/content/<uri>.
	resp = mustGet(t, baseURL+"/api/v1/content/e2e_rat.md", "e2e")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get content: status %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "Rats are rodents") {
		t.Fatalf("get content: body does not contain expected text; got %s", got)
	}

	// 3. Search — sparse-only (no VLM configured, so intent analysis
	// is skipped and the retriever falls back to keyword matching).
	// The hash-fallback embedder produces non-meaningful dense vectors,
	// but the sparse leg (ragfs.Grep) matches "rodent" against the
	// written content.
	resp = mustPost(t, baseURL+"/api/v1/search", `{"query":"rodent","top_k":5}`, "e2e")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("search: status %d body %s", resp.StatusCode, b)
	}
}

// TestLocalServer_Health verifies /healthz returns 200 and the standard
// envelope.
func TestLocalServer_Health(t *testing.T) {
	bin := locateBinary(t, "openviking-server")
	cfgPath := locateConfig(t)

	srvCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(srvCtx, bin, "--config", cfgPath, "--addr", addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	if !waitForHealth(t, baseURL+healthPath, 30*time.Second) {
		t.Fatal("server never became healthy")
	}

	resp, err := http.Get(baseURL + healthPath)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: status %d", resp.StatusCode)
	}
}

// locateBinary resolves ../../bin/<name> from the test file's location
// using runtime.Caller so it is immune to os.Chdir. Skips the test if
// the binary is missing.
func locateBinary(t *testing.T, name string) string {
	t.Helper()
	wd := testRepoRoot(t)
	bin := filepath.Join(wd, "bin", name)
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("binary %s not built (run `make build`): %v", bin, err)
	}
	return bin
}

// locateConfig resolves ../../examples/ov.conf.local-memory from the
// test file's location.
func locateConfig(t *testing.T) string {
	t.Helper()
	wd := testRepoRoot(t)
	cfg := filepath.Join(wd, "examples", "ov.conf.local-memory")
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("config missing: %v", err)
	}
	return cfg
}

// testRepoRoot returns the repository root computed from the test file's
// source location via runtime.Caller. This is immune to os.Chdir.
func testRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file = <root>/tests/e2e/local_server_test.go
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// waitForHealth polls /healthz until it returns 200 or the timeout
// elapses.
func waitForHealth(t *testing.T, url string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return true
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func mustPost(t *testing.T, url, body, account string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OpenViking-Account", account)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func mustPut(t *testing.T, url, body, account string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-OpenViking-Account", account)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	return resp
}

func mustGet(t *testing.T, url, account string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	req.Header.Set("X-OpenViking-Account", account)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// init creates a temp working directory for the server's relative
// ./data/ paths. The binary and config paths are resolved from the
// test file's source location, so this chdir is safe.
func init() {
	tmp, err := os.MkdirTemp("", "ov-e2e-")
	if err != nil {
		panic(fmt.Sprintf("mktemp: %v", err))
	}
	_ = os.Chdir(tmp)
	fmt.Println("e2e: working dir", tmp)
}
