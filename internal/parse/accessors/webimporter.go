// Package accessors — WebImporter.
//
// WebImporter is the Go counterpart of openviking.parse.accessors.web_importer.
// It crawls ordinary web pages and writes them as a directory tree of HTML
// files under <tmp>/<host>/. The result can be fed into the directory parser
// (parsers.DirectoryParser) like any other LocalResource.
//
// Unlike StaticAccessor (which exposes only depth + limit), WebImporter
// exposes the full Python WebImportOptions surface: include_paths /
// exclude_paths (URL-path glob filters), allow_external_links, and
// skip_download_links. The download-link handling is intentionally not
// reimplemented here — colly's HTML callback emits anchor links to the
// page tree only; binary downloads are a Python-side concern routed through
// HTTPAccessor. Tracked as a known gap in docs/design/go-rewrite-known-gaps.md
// (§1.9).
package accessors

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// WebImportOptions controls WebImporter behavior. Mirrors the Python
// openviking.parse.accessors.web_importer.WebImportOptions dataclass.
//
// Field defaults (when zero):
//
//	Depth       : 3 (capped default)
//	MaxPages    : 50 (capped default)
//	IncludePaths: nil = no include filter
//	ExcludePaths: nil = no exclude filter
type WebImportOptions struct {
	// Depth limits crawl depth. 0 means default (3). -1 means unlimited
	// (mapped to a high cap, since colly has no "unlimited" mode).
	Depth int
	// MaxPages caps the number of pages written. 0 means default (50).
	// -1 means unlimited (mapped to a high cap).
	MaxPages int
	// IncludePaths is a list of glob patterns; only URLs whose path
	// matches at least one pattern are kept. Empty means "all paths".
	IncludePaths []string
	// ExcludePaths is a list of glob patterns; URLs whose path matches
	// any pattern are dropped. Empty means "no exclusions".
	ExcludePaths []string
	// AllowExternalLinks controls whether links to other hosts are
	// followed. When false (default), the crawler is restricted to the
	// root URL's host.
	AllowExternalLinks bool
	// SkipDownloadLinks controls whether obvious download links (PDF,
	// ZIP, etc.) are skipped during crawl. Default true. Currently a
	// no-op in the Go port — see the package doc.
	SkipDownloadLinks bool
}

// WebImportResult is the output of a successful web import. Path is the
// host directory under <tmp>/; Meta carries counters and the original
// host name for the parser layer.
type WebImportResult struct {
	Path string
	Meta map[string]any
}

// WebImporter crawls ordinary web pages and writes them as HTML files.
// Construct one directly or via the "web_import" accessor scheme.
type WebImporter struct {
	UserAgent string
}

// Default constants. Match Python's defaults where applicable.
const (
	webImportDefaultDepth    = 3
	webImportDefaultMaxPages = 50
	webImportUnlimitedCap    = 10000
)

// CanHandle reports whether source is an http/https URL. The web_import
// scheme is dispatched by URL form ("web_import://https://...") or by
// callers passing opts.Site=true via the crawl scheme; this accessor
// accepts any http/https source.
func (w *WebImporter) CanHandle(source string, _ parse.AccessorOptions) bool {
	s := parse.SchemeOf(source)
	return s == "http" || s == "https"
}

// Schemes returns the URI schemes this accessor handles.
func (w *WebImporter) Schemes() []string { return []string{"web_import"} }

// Fetch is the DataAccessor entry point. It maps AccessorOptions to
// WebImportOptions and delegates to ImportToDirectory. The returned
// LocalResource is owned by the caller and must be Cleanup'd.
func (w *WebImporter) Fetch(ctx context.Context, source string, opts parse.AccessorOptions) (*parse.LocalResource, error) {
	wopts := WebImportOptions{
		Depth:    opts.Depth,
		MaxPages: opts.Limit,
	}
	res, err := w.ImportToDirectory(ctx, source, wopts)
	if err != nil {
		return nil, err
	}
	return &parse.LocalResource{
		Path:           res.Path,
		SourceType:     parse.SourceWebCrawler,
		OriginalSource: source,
		IsTemporary:    true,
		Meta:           res.Meta,
	}, nil
}

// ImportToDirectory crawls rootURL up to opts.Depth and writes pages under
// <tmp>/<host>/. Returns a result pointing at the host directory. The
// caller owns the directory and must remove it when done.
func (w *WebImporter) ImportToDirectory(ctx context.Context, rootURL string, opts WebImportOptions) (*WebImportResult, error) {
	u, err := url.Parse(rootURL)
	if err != nil || u.Host == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"webimporter: invalid root URL")
	}
	depth := normalizeDepth(opts.Depth)
	maxPages := normalizeMaxPages(opts.MaxPages)

	tmp, err := os.MkdirTemp("", "ov_web_*")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	hostDir := filepath.Join(tmp, sanitizeHost(u.Host))
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}

	ua := w.UserAgent
	if ua == "" {
		ua = "OpenViking-WebImporter/1.0"
	}

	c := colly.NewCollector(
		colly.UserAgent(ua),
		colly.MaxDepth(depth),
		colly.Async(false),
	)
	c.SetRequestTimeout(30 * time.Second)
	if !opts.AllowExternalLinks {
		c.AllowedDomains = []string{hostnameOnly(u.Host)}
	}

	visited := 0
	entryOK := false
	// Follow anchor links so multi-page crawls actually traverse the
	// site. colly does not auto-follow links; the OnHTML("a[href]")
	// callback is the canonical place to enqueue them. URL-path and
	// host filters are applied at fetch time via OnRequest and the
	// AllowedDomains collector option.
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		if visited >= maxPages {
			return
		}
		link := e.Attr("href")
		if link == "" {
			return
		}
		// Resolve relative links against the page URL. Skip fragments
		// and javascript: URIs.
		abs := e.Request.AbsoluteURL(link)
		if abs == "" {
			return
		}
		if !opts.AllowExternalLinks {
			pu, err := url.Parse(abs)
			if err != nil || !sameHost(pu.Host, u.Host) {
				return
			}
		}
		_ = e.Request.Visit(abs)
	})
	c.OnHTML("html", func(e *colly.HTMLElement) {
		if visited >= maxPages {
			return
		}
		pageURL := e.Request.URL.String()
		if !urlPathAllowed(e.Request.URL.Path, opts.IncludePaths, opts.ExcludePaths) {
			return
		}
		visited++
		if !entryOK && sameURL(pageURL, rootURL) {
			entryOK = true
		}
		rel := pathForURL(pageURL, u.Host)
		out := filepath.Join(hostDir, rel)
		if !strings.HasSuffix(out, ".html") {
			out += ".html"
		}
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		_ = os.WriteFile(out, []byte(e.Response.Body), 0o644)
	})
	c.OnRequest(func(r *colly.Request) {
		if visited >= maxPages {
			r.Abort()
		}
	})
	c.OnError(func(_ *colly.Response, err error) {
		// Surface crawl errors via the entry-page check below; per-page
		// errors are silently swallowed to match Python's dedupe behavior.
		_ = err
	})

	if err := c.Visit(rootURL); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, domain.Wrap(domain.CodeInternalError, 502,
			fmt.Errorf("webimporter: visit %s: %w", rootURL, err))
	}
	c.Wait()

	if !entryOK {
		_ = os.RemoveAll(tmp)
		return nil, domain.NewAppError(domain.CodeInternalError, 502,
			fmt.Sprintf("webimporter: failed to fetch entry page %s", rootURL))
	}

	return &WebImportResult{
		Path: hostDir,
		Meta: map[string]any{
			"web_import":        true,
			"page_count":        visited,
			"host":              u.Host,
			"depth":             depth,
			"max_pages":         maxPages,
			"original_filename": sanitizeHost(u.Host),
		},
	}, nil
}

// normalizeDepth maps user-facing Depth to a colly-usable value. 0 ->
// default (3). -1 -> high cap (colly has no "unlimited" mode; 10000 is
// effectively unlimited for human-scale sites).
func normalizeDepth(d int) int {
	switch {
	case d == 0:
		return webImportDefaultDepth
	case d < 0:
		return webImportUnlimitedCap
	default:
		return d
	}
}

// normalizeMaxPages maps user-facing MaxPages to an internal cap. 0 ->
// default (50). -1 -> high cap.
func normalizeMaxPages(m int) int {
	switch {
	case m == 0:
		return webImportDefaultMaxPages
	case m < 0:
		return webImportUnlimitedCap
	default:
		return m
	}
}

// urlPathAllowed reports whether path passes the include/exclude filters.
// When include is non-empty, path must match at least one pattern. When
// exclude is non-empty, path must not match any pattern. Patterns are
// glob-style (e.g. "/docs/*", "/api/v[0-9]/*").
func urlPathAllowed(path string, include, exclude []string) bool {
	if len(exclude) > 0 && matchAnyGlob(path, exclude) {
		return false
	}
	if len(include) > 0 && !matchAnyGlob(path, include) {
		return false
	}
	return true
}

// matchAnyGlob reports whether s matches any pattern in patterns.
// Glob matching uses filepath.Match semantics.
func matchAnyGlob(s string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, s); ok {
			return true
		}
	}
	return false
}

// sanitizeHost replaces path-unsafe characters in a hostname with "_".
func sanitizeHost(h string) string {
	h = strings.ToLower(h)
	h = strings.TrimRight(h, ".")
	if h == "" {
		return "web"
	}
	h = strings.ReplaceAll(h, string(filepath.Separator), "_")
	h = strings.ReplaceAll(h, ":", "_")
	return h
}

// hostnameOnly strips the port from a host:port string.
func hostnameOnly(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

// sameHost reports whether two host[:port] strings refer to the same
// host (port-agnostic). Used to gate external-link following.
func sameHost(a, b string) bool {
	return hostnameOnly(strings.ToLower(a)) == hostnameOnly(strings.ToLower(b))
}

// sameURL reports whether two URL strings refer to the same page after
// trivial normalization (trailing slash, fragment stripping).
func sameURL(a, b string) bool {
	return normalizeURL(a) == normalizeURL(b)
}

// normalizeURL lowercases scheme/host, drops the fragment, and ensures
// the path is non-empty.
func normalizeURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u == nil {
		return s
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

// pathForURL returns the file-system path for a fetched page URL relative
// to host root. Empty path becomes "index"; query is appended as a hash
// suffix to disambiguate URLs that share a path.
func pathForURL(rawURL, _ string) string {
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

// hashShort returns an 8-char hex digest of s. Used to disambiguate URLs
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
