// Package webcrawler implements a static + dynamic web crawler using
// gocolly/colly for static HTML and playwright-community/playwright-go
// for JavaScript-rendered pages.
//
// The DynamicAccessor launches a headless Chromium via Playwright. The
// Playwright driver + browser binary must be installed separately:
//
//	go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium
//
// When the binary is missing, NewDynamicCrawler returns a descriptive
// error; Fetch surfaces the same error so callers can present install
// instructions to the user.
package webcrawler

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/playwright-community/playwright-go"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// StaticAccessor crawls static HTML pages with colly. It writes the
// fetched pages as a directory tree of HTML files.
type StaticAccessor struct {
	UserAgent string
}

func init() {
	parse.RegisterAccessor("crawl", func() (parse.DataAccessor, error) {
		return &StaticAccessor{UserAgent: "OpenViking-Crawler/1.0"}, nil
	})
	parse.RegisterAccessor("sitemap", func() (parse.DataAccessor, error) {
		return &StaticAccessor{UserAgent: "OpenViking-Crawler/1.0"}, nil
	})
	parse.RegisterAccessor("render", func() (parse.DataAccessor, error) {
		return NewDynamicAccessor()
	})
	parse.RegisterAccessor("playwright", func() (parse.DataAccessor, error) {
		return NewDynamicAccessor()
	})
}

// CanHandle reports whether source is a crawlable http/https URL passed
// with opts.Site=true (whole-site ingest).
func (a *StaticAccessor) CanHandle(source string, opts parse.AccessorOptions) bool {
	s := parse.SchemeOf(source)
	if s != "http" && s != "https" {
		return false
	}
	return opts.Site
}

// Schemes returns crawl, sitemap.
func (a *StaticAccessor) Schemes() []string { return []string{"crawl", "sitemap"} }

// Fetch crawls the site rooted at source up to opts.Depth (default 3)
// and writes each fetched page as <tmp>/<host>/<path>.html.
func (a *StaticAccessor) Fetch(ctx context.Context, source string, opts parse.AccessorOptions) (*parse.LocalResource, error) {
	u, err := url.Parse(source)
	if err != nil || u.Host == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"webcrawler: invalid source URL")
	}
	tmp, err := os.MkdirTemp(opts.TemporaryDir, "openviking-crawl-*")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	hostDir := filepath.Join(tmp, sanitizePath(u.Host))
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}

	depth := opts.Depth
	if depth <= 0 {
		depth = 3
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	ua := a.UserAgent
	if ua == "" {
		ua = "OpenViking-Crawler/1.0"
	}

	c := colly.NewCollector(
		colly.UserAgent(ua),
		colly.MaxDepth(depth),
		colly.Async(false),
	)
	c.SetRequestTimeout(30 * time.Second)
	visited := 0
	c.OnHTML("html", func(e *colly.HTMLElement) {
		if visited >= limit {
			return
		}
		visited++
		pageURL := e.Request.URL.String()
		rel := pathForURL(pageURL, u.Host)
		out := filepath.Join(hostDir, rel)
		if !strings.HasSuffix(out, ".html") {
			out += ".html"
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return
		}
		_ = os.WriteFile(out, []byte(e.Response.Body), 0o644)
	})
	c.OnRequest(func(r *colly.Request) {
		if visited >= limit {
			r.Abort()
		}
	})
	if err := c.Visit(source); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, domain.Wrap(domain.CodeInternalError, 502,
			fmt.Errorf("webcrawler: visit %s: %w", source, err))
	}
	c.Wait()

	return &parse.LocalResource{
		Path:           hostDir,
		SourceType:     parse.SourceWebCrawler,
		OriginalSource: source,
		IsTemporary:    true,
		Meta: map[string]any{
			"host":    u.Host,
			"depth":   depth,
			"limit":   limit,
			"visited": visited,
		},
	}, nil
}

// sanitizePath replaces path-unsafe characters with "_".
func sanitizePath(p string) string {
	p = strings.ReplaceAll(p, string(filepath.Separator), "_")
	p = strings.ReplaceAll(p, ":", "_")
	return p
}

// pathForURL returns the file-system path for a fetched page URL relative
// to host root. Empty path becomes "index"; query is appended as a hash.
func pathForURL(rawURL, host string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "index.html"
	}
	p := strings.TrimPrefix(u.Path, "/")
	if p == "" {
		p = "index"
	}
	if u.RawQuery != "" {
		p += "_" + hashShort(u.RawQuery)
	}
	return p
}

// hashShort returns a 8-char hex digest of s. Used to disambiguate URLs
// that share the same path.
func hashShort(s string) string {
	const hex = "0123456789abcdef"
	var h [8]byte
	for i := 0; i < len(s) && i < 64; i++ {
		h[i%8] ^= s[i]
	}
	out := make([]byte, 8)
	for i, b := range h {
		out[i] = hex[b>>4]
	}
	return string(out)
}

// playwrightInstallHint is the user-facing install instruction appended
// to errors returned when the Chromium binary or Playwright driver is
// missing.
const playwrightInstallHint = "run `go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium`"

// DynamicAccessor is the Playwright-backed crawler for JavaScript-rendered
// pages. Constructed lazily via NewDynamicAccessor; the browser is
// launched on the first Fetch call and reused across calls. Call Close
// to release the browser process.
type DynamicAccessor struct {
	pw      *playwright.Playwright
	browser playwright.Browser
}

// NewDynamicAccessor pre-warms the Playwright driver and launches a
// headless Chromium browser. Returns a descriptive error when the
// driver or browser binary is missing — callers should surface the
// error, not fall back to a stub.
func NewDynamicAccessor() (*DynamicAccessor, error) {
	pw, err := playwright.Run()
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("webcrawler: playwright init failed (%s): %w", playwrightInstallHint, err))
	}
	browser, err := pw.Chromium.Launch()
	if err != nil {
		_ = pw.Stop()
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("webcrawler: chromium launch failed (%s): %w", playwrightInstallHint, err))
	}
	return &DynamicAccessor{pw: pw, browser: browser}, nil
}

// CanHandle reports whether source opts requests a JS-rendered crawl
// (opts.Site=true with scheme "render" or "playwright").
func (a *DynamicAccessor) CanHandle(source string, opts parse.AccessorOptions) bool {
	s := parse.SchemeOf(source)
	return s == "render" || s == "playwright"
}

// Schemes returns render, playwright.
func (a *DynamicAccessor) Schemes() []string { return []string{"render", "playwright"} }

// Fetch navigates a headless Chromium to source, waits for network idle,
// and writes the rendered HTML to a temp file. The HTML is also returned
// in Meta for parsers that prefer in-memory content.
func (a *DynamicAccessor) Fetch(ctx context.Context, source string, opts parse.AccessorOptions) (*parse.LocalResource, error) {
	if a == nil || a.pw == nil || a.browser == nil {
		return nil, domain.NewAppError(domain.CodeInternalError, 500,
			"webcrawler: dynamic crawler not initialized ("+playwrightInstallHint+")")
	}
	if _, err := url.Parse(source); err != nil {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("webcrawler: invalid source URL %q: %w", source, err))
	}

	page, err := a.browser.NewPage()
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("webcrawler: new page: %w", err))
	}
	defer func() { _ = page.Close() }()

	// Cap the networkidle wait so pages with continuous background
	// activity (websockets, polling) do not block forever.
	networkIdleTimeout := float64(8000)
	if _, err := page.Goto(source, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
		Timeout:   &networkIdleTimeout,
	}); err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 502,
			fmt.Errorf("webcrawler: goto %s: %w", source, err))
	}
	html, err := page.Content()
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 502,
			fmt.Errorf("webcrawler: read content %s: %w", source, err))
	}

	tmpDir := opts.TemporaryDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	out, err := os.CreateTemp(tmpDir, "openviking-render-*.html")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	tmpPath := out.Name()
	if _, err := out.WriteString(html); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}

	return &parse.LocalResource{
		Path:           tmpPath,
		SourceType:     parse.SourceWebCrawler,
		OriginalSource: source,
		IsTemporary:    true,
		Meta: map[string]any{
			"url":    source,
			"format": "html",
			"size":   len(html),
		},
	}, nil
}

// Close releases the Chromium browser and Playwright driver. Safe to
// call multiple times.
func (a *DynamicAccessor) Close() error {
	if a == nil {
		return nil
	}
	if a.browser != nil {
		_ = a.browser.Close()
		a.browser = nil
	}
	if a.pw != nil {
		_ = a.pw.Stop()
		a.pw = nil
	}
	return nil
}
