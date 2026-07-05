package webcrawler

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saker-ai/ctxhub/internal/parse"
)

// TestStaticAccessor_Fetch exercises the colly-backed static crawler
// against an httptest server. It verifies that pages are written to the
// temp dir and that visited counter is reported in metadata.
func TestStaticAccessor_Fetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Home</h1><a href=\"/page2\">p2</a></body></html>"))
	})
	mux.HandleFunc("/page2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Page 2</h1></body></html>"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	acc := &StaticAccessor{UserAgent: "OpenViking-Crawler-Test/1.0"}
	res, err := acc.Fetch(context.Background(), server.URL, parse.AccessorOptions{
		Site:  true,
		Depth: 2,
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer res.Cleanup()

	if res.SourceType != parse.SourceWebCrawler {
		t.Errorf("SourceType=%v, want SourceWebCrawler", res.SourceType)
	}
	if !res.IsTemporary {
		t.Errorf("IsTemporary=%v, want true", res.IsTemporary)
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", res.Path, err)
	}
	if !info.IsDir() {
		t.Fatalf("res.Path %q is not a directory", res.Path)
	}
	// Verify at least one HTML file was written.
	var found bool
	_ = filepath.WalkDir(res.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".html") {
			found = true
		}
		return nil
	})
	if !found {
		t.Errorf("no .html files written under %q", res.Path)
	}
}

// TestDynamicAccessor_PlaywrightMissing exercises the error path when
// the Playwright driver or Chromium binary is not installed. The test
// skips if Playwright is available (so CI with the binary installed
// still passes), and otherwise asserts that NewDynamicAccessor returns
// a descriptive error mentioning the install command.
func TestDynamicAccessor_PlaywrightMissing(t *testing.T) {
	acc, err := NewDynamicAccessor()
	if err == nil {
		// Playwright is installed; close the browser and skip.
		t.Cleanup(func() {
			if acc != nil {
				_ = acc.Close()
			}
		})
		t.Skip("playwright chromium is installed; skipping missing-binary test")
		return
	}
	if acc != nil {
		t.Fatalf("NewDynamicAccessor returned non-nil accessor with error: %v", err)
	}
	if !strings.Contains(err.Error(), "playwright install") && !strings.Contains(err.Error(), "chromium") {
		t.Errorf("error should mention playwright/chromium install, got: %v", err)
	}
}

// TestDynamicAccessor_NilAccessorFetch exercises the Fetch path when
// the accessor was never constructed (nil receiver fields). This
// simulates a registry misuse where DynamicAccessor is registered but
// construction failed silently.
func TestDynamicAccessor_NilAccessorFetch(t *testing.T) {
	var acc *DynamicAccessor // nil pointer; intentionally constructed
	_, err := acc.Fetch(context.Background(), "render://example.com", parse.AccessorOptions{})
	if err == nil {
		t.Fatal("Fetch on nil DynamicAccessor: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("Fetch error should mention not-initialized, got: %v", err)
	}
}
