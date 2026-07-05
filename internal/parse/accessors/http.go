package accessors

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// HTTPAccessor fetches HTTP/HTTPS URLs into a temp file. It retries
// transient failures (5xx and network errors) with exponential backoff.
type HTTPAccessor struct {
	Client *http.Client
}

func init() {
	parse.RegisterAccessor("http", func() (parse.DataAccessor, error) {
		return &HTTPAccessor{Client: defaultHTTPClient()}, nil
	})
	parse.RegisterAccessor("https", func() (parse.DataAccessor, error) {
		return &HTTPAccessor{Client: defaultHTTPClient()}, nil
	})
}

// defaultHTTPClient returns a sane default *http.Client with timeouts.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("http: stopped after 10 redirects")
			}
			return nil
		},
	}
}

// CanHandle reports whether source is an http/https URL.
func (a *HTTPAccessor) CanHandle(source string, opts parse.AccessorOptions) bool {
	s := parse.SchemeOf(source)
	return s == "http" || s == "https"
}

// Schemes returns http and https.
func (a *HTTPAccessor) Schemes() []string { return []string{"http", "https"} }

// Fetch downloads source into a temp file with up to 3 retries.
func (a *HTTPAccessor) Fetch(ctx context.Context, source string, opts parse.AccessorOptions) (*parse.LocalResource, error) {
	client := a.Client
	if client == nil {
		client = defaultHTTPClient()
	}
	tmpDir := opts.TemporaryDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	name := httpBaseName(source)
	if name == "" {
		name = "download.bin"
	}
	out, err := os.CreateTemp(tmpDir, "openviking-http-*-"+name)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	tmpPath := out.Name()
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}

	var lastErr error
	backoff := time.Second
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			_ = os.Remove(tmpPath)
			return nil, err
		}
		err := a.downloadOnce(ctx, client, source, tmpPath)
		if err == nil {
			info, _ := os.Stat(tmpPath)
			meta := map[string]any{
				"url":     source,
				"attempt": attempt + 1,
			}
			if info != nil {
				meta["size"] = info.Size()
			}
			return &parse.LocalResource{
				Path:           tmpPath,
				SourceType:     parse.SourceHTTP,
				OriginalSource: source,
				IsTemporary:    true,
				Meta:           meta,
			}, nil
		}
		lastErr = err
		if !isRetryable(err) {
			break
		}
		select {
		case <-ctx.Done():
			_ = os.Remove(tmpPath)
			return nil, ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
		}
	}
	_ = os.Remove(tmpPath)
	return nil, domain.Wrap(domain.CodeInternalError, 502,
		fmt.Errorf("http accessor: %s: %w", source, lastErr))
}

// downloadOnce performs a single HTTP GET and streams the body into out.
func (a *HTTPAccessor) downloadOnce(ctx context.Context, client *http.Client, url, out string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "OpenViking-Parser/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("http: server returned %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http: client error %d", resp.StatusCode)
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// isRetryable reports whether an HTTP error is worth retrying.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "server returned 5") {
		return true
	}
	if strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "no such host") {
		return true
	}
	return false
}

// httpBaseName extracts the file name from a URL path. Returns "" when
// the path has no base name (e.g. trailing slash).
func httpBaseName(url string) string {
	// Strip query and fragment.
	clean := url
	if idx := strings.IndexByte(clean, '?'); idx >= 0 {
		clean = clean[:idx]
	}
	if idx := strings.IndexByte(clean, '#'); idx >= 0 {
		clean = clean[:idx]
	}
	clean = strings.TrimRight(clean, "/")
	if clean == "" {
		return ""
	}
	if idx := strings.LastIndexByte(clean, '/'); idx >= 0 {
		clean = clean[idx+1:]
	}
	if clean == "" {
		return ""
	}
	// Disallow obvious unsafe characters.
	if strings.ContainsAny(clean, ":\\") {
		return "download.bin"
	}
	return filepath.Base(clean)
}
