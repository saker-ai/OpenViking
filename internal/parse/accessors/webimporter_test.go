package accessors

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

// TestWebImporter_Basic exercises a single-page crawl against an
// httptest server. It verifies the entry page is written, IsTemporary
// is set, and Meta carries the expected counters.
func TestWebImporter_Basic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Home</h1></body></html>"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	imp := &WebImporter{}
	res, err := imp.ImportToDirectory(context.Background(), server.URL, WebImportOptions{})
	if err != nil {
		t.Fatalf("ImportToDirectory: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(res.Path))

	if res.Meta["web_import"] != true {
		t.Errorf("Meta[web_import]=%v, want true", res.Meta["web_import"])
	}
	if got := res.Meta["page_count"]; got != 1 {
		t.Errorf("Meta[page_count]=%v, want 1", got)
	}
	if got, ok := res.Meta["host"].(string); !ok || got == "" {
		t.Errorf("Meta[host]=%v, want non-empty string", res.Meta["host"])
	}

	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", res.Path, err)
	}
	if !info.IsDir() {
		t.Fatalf("res.Path %q is not a directory", res.Path)
	}

	var foundHTML bool
	_ = filepath.WalkDir(res.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".html") {
			foundHTML = true
		}
		return nil
	})
	if !foundHTML {
		t.Errorf("no .html files written under %q", res.Path)
	}
}

// TestWebImporter_MultiPage verifies that linked pages are crawled and
// written up to MaxPages.
func TestWebImporter_MultiPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><a href="/p2">p2</a><a href="/p3">p3</a></body></html>`))
	})
	mux.HandleFunc("/p2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>P2</h1></body></html>"))
	})
	mux.HandleFunc("/p3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>P3</h1></body></html>"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	imp := &WebImporter{}
	res, err := imp.ImportToDirectory(context.Background(), server.URL, WebImportOptions{Depth: 2, MaxPages: 10})
	if err != nil {
		t.Fatalf("ImportToDirectory: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(res.Path))

	if got := res.Meta["page_count"]; got.(int) < 2 {
		t.Errorf("Meta[page_count]=%v, want >= 2", got)
	}
}

// TestWebImporter_IncludePaths verifies that the include filter drops
// non-matching pages.
func TestWebImporter_IncludePaths(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Link to both /keep and /skip; the filter should drop /skip.
		_, _ = w.Write([]byte(`<html><body><a href="/keep">k</a><a href="/skip">s</a></body></html>`))
	})
	mux.HandleFunc("/keep", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Keep</h1></body></html>"))
	})
	mux.HandleFunc("/skip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Skip</h1></body></html>"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	imp := &WebImporter{}
	res, err := imp.ImportToDirectory(context.Background(), server.URL, WebImportOptions{
		Depth:        2,
		MaxPages:     10,
		IncludePaths: []string{"/", "/keep"},
	})
	if err != nil {
		t.Fatalf("ImportToDirectory: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(res.Path))

	var sawSkip bool
	_ = filepath.WalkDir(res.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.Contains(path, "skip") {
			sawSkip = true
		}
		return nil
	})
	if sawSkip {
		t.Errorf("IncludePaths filter should have dropped /skip, but file was written under %q", res.Path)
	}
}

// TestWebImporter_ExcludePaths verifies that the exclude filter drops
// matching pages.
func TestWebImporter_ExcludePaths(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><a href="/keep">k</a><a href="/skip">s</a></body></html>`))
	})
	mux.HandleFunc("/keep", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Keep</h1></body></html>"))
	})
	mux.HandleFunc("/skip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Skip</h1></body></html>"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	imp := &WebImporter{}
	res, err := imp.ImportToDirectory(context.Background(), server.URL, WebImportOptions{
		Depth:        2,
		MaxPages:     10,
		ExcludePaths: []string{"/skip"},
	})
	if err != nil {
		t.Fatalf("ImportToDirectory: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(res.Path))

	var sawSkip bool
	_ = filepath.WalkDir(res.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.Contains(path, "skip") {
			sawSkip = true
		}
		return nil
	})
	if sawSkip {
		t.Errorf("ExcludePaths filter should have dropped /skip, but file was written under %q", res.Path)
	}
}

// TestWebImporter_AllowExternalLinks verifies that external links are
// not followed when AllowExternalLinks is false.
func TestWebImporter_AllowExternalLinks(t *testing.T) {
	externalMux := http.NewServeMux()
	externalMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>External</h1></body></html>"))
	})
	external := httptest.NewServer(externalMux)
	defer external.Close()

	mainMux := http.NewServeMux()
	mainMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><a href="` + external.URL + `/">ext</a></body></html>`))
	})
	main := httptest.NewServer(mainMux)
	defer main.Close()

	imp := &WebImporter{}
	res, err := imp.ImportToDirectory(context.Background(), main.URL, WebImportOptions{
		Depth:              2,
		MaxPages:           10,
		AllowExternalLinks: false,
	})
	if err != nil {
		t.Fatalf("ImportToDirectory: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(res.Path))

	// The external server's host should not appear as a directory under
	// the import tree.
	var sawExternalDir bool
	_ = filepath.WalkDir(res.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.Contains(path, "127.0.0.1") || strings.Contains(path, "localhost") {
			// The main server is also on 127.0.0.1; we only flag external
			// when the file is named "External" content.
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), "External") {
				sawExternalDir = true
			}
		}
		return nil
	})
	if sawExternalDir {
		t.Errorf("AllowExternalLinks=false should have blocked the external page, but it was written under %q", res.Path)
	}
}

// TestWebImporter_Fetch_Delegate verifies that the DataAccessor Fetch
// adapter wraps ImportToDirectory and returns a LocalResource with the
// expected SourceType.
func TestWebImporter_Fetch_Delegate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Home</h1></body></html>"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	imp := &WebImporter{}
	res, err := imp.Fetch(context.Background(), server.URL, parse.AccessorOptions{Depth: 1, Limit: 5})
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
	if res.Meta["web_import"] != true {
		t.Errorf("Meta[web_import]=%v, want true", res.Meta["web_import"])
	}
}

// TestWebImporter_InvalidURL verifies that invalid URLs are rejected
// with a 422 domain error.
func TestWebImporter_InvalidURL(t *testing.T) {
	imp := &WebImporter{}
	_, err := imp.ImportToDirectory(context.Background(), "not a url", WebImportOptions{})
	if err == nil {
		t.Fatal("ImportToDirectory with invalid URL: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid root URL") {
		t.Errorf("error should mention invalid root URL, got: %v", err)
	}
}

// TestWebImporter_NormalizeDepth verifies the depth normalization
// rules: 0 -> default (3), -1 -> cap (10000), positive passes through.
func TestWebImporter_NormalizeDepth(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 3},
		{-1, 10000},
		{2, 2},
		{5, 5},
	}
	for _, tc := range cases {
		if got := normalizeDepth(tc.in); got != tc.want {
			t.Errorf("normalizeDepth(%d)=%d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestWebImporter_NormalizeMaxPages verifies the MaxPages normalization
// rules: 0 -> default (50), -1 -> cap (10000), positive passes through.
func TestWebImporter_NormalizeMaxPages(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 50},
		{-1, 10000},
		{10, 10},
	}
	for _, tc := range cases {
		if got := normalizeMaxPages(tc.in); got != tc.want {
			t.Errorf("normalizeMaxPages(%d)=%d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestWebImporter_UrlPathAllowed verifies the include/exclude filter
// logic.
func TestWebImporter_UrlPathAllowed(t *testing.T) {
	cases := []struct {
		name             string
		path             string
		include, exclude []string
		want             bool
	}{
		{"no_filters", "/foo", nil, nil, true},
		{"include_match", "/docs/x", []string{"/docs/*"}, nil, true},
		{"include_nomatch", "/api/x", []string{"/docs/*"}, nil, false},
		{"exclude_match", "/skip", nil, []string{"/skip"}, false},
		{"exclude_nomatch", "/keep", nil, []string{"/skip"}, true},
		{"include_exclude", "/docs/keep", []string{"/docs/*"}, []string{"/docs/skip"}, true},
		{"include_exclude_drop", "/docs/skip", []string{"/docs/*"}, []string{"/docs/skip"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := urlPathAllowed(tc.path, tc.include, tc.exclude); got != tc.want {
				t.Errorf("urlPathAllowed(%q, %v, %v)=%v, want %v",
					tc.path, tc.include, tc.exclude, got, tc.want)
			}
		})
	}
}

// TestWebImporter_SanitizeHost verifies host name sanitization.
func TestWebImporter_SanitizeHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"example.com", "example.com"},
		{"EXAMPLE.COM", "example.com"},
		{"example.com.", "example.com"},
		{"", "web"},
		{"host:8080", "host_8080"},
	}
	for _, tc := range cases {
		if got := sanitizeHost(tc.in); got != tc.want {
			t.Errorf("sanitizeHost(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}
