package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/xid"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// HTTPDoer is the minimum HTTP interface Client needs. *http.Client
// satisfies it; tests substitute a stub.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is the OpenViking server client used by every CLI subcommand.
// It is safe for concurrent use once configured.
type Client struct {
	baseURL    string
	httpClient HTTPDoer
	token      string // bearer token from OAuth; empty = unauthenticated
	account    string // X-OpenViking-Account header
	user       string // X-OpenViking-User header (optional)
	userAgent  string
}

// ClientOption configures a Client at construction time.
type ClientOption func(*Client)

// WithHTTPClient replaces the default *http.Client used for requests.
// Tests use this to inject a stub HTTPDoer.
func WithHTTPClient(c HTTPDoer) ClientOption {
	return func(cl *Client) { cl.httpClient = c }
}

// WithToken sets the bearer token sent on every request.
func WithToken(t string) ClientOption {
	return func(cl *Client) { cl.token = t }
}

// WithAccount sets the X-OpenViking-Account header. Required when the
// server's identity middleware is enabled.
func WithAccount(a string) ClientOption {
	return func(cl *Client) { cl.account = a }
}

// WithUser sets the optional X-OpenViking-User header.
func WithUser(u string) ClientOption {
	return func(cl *Client) { cl.user = u }
}

// WithUserAgent sets the User-Agent header sent on every request.
func WithUserAgent(ua string) ClientOption {
	return func(cl *Client) { cl.userAgent = ua }
}

// NewClient constructs a Client targeting baseURL with sensible defaults.
func NewClient(baseURL string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		account:   "default",
		userAgent: "ov-cli/1.0",
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// BaseURL returns the configured server base URL (no trailing slash).
func (c *Client) BaseURL() string { return c.baseURL }

// Account returns the account header value.
func (c *Client) Account() string { return c.account }

// SetToken replaces the bearer token at runtime (used by auth refresh).
func (c *Client) SetToken(t string) { c.token = t }

// Do issues an HTTP request against the server. path must begin with "/"
// and is joined onto baseURL. body may be nil. The caller closes resp.Body.
//
// Do attaches the standard OpenViking headers:
//   - X-OpenViking-Account (always; default "default")
//   - X-OpenViking-User (when set)
//   - Authorization: Bearer <token> (when token set)
//   - X-Request-ID (always; generated via rs/xid)
//   - User-Agent
//
// On a 4xx/5xx response with a JSON error body, Do returns an
// *domain.AppError decoded from the {error: {code, message, details}}
// envelope. The response body is closed in this case.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.doWithContentType(ctx, method, path, body, "")
}

// doWithContentType is the inner request builder. contentType is set
// on the request when non-empty; pass "" to omit.
func (c *Client) doWithContentType(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("cli: path must start with /, got %q", path)
	}
	reqURL, err := url.Parse(c.baseURL + path)
	if err != nil {
		return nil, fmt.Errorf("cli: parse url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), body)
	if err != nil {
		return nil, fmt.Errorf("cli: build request: %w", err)
	}
	if c.account != "" {
		req.Header.Set("X-OpenViking-Account", c.account)
	}
	if c.user != "" {
		req.Header.Set("X-OpenViking-User", c.user)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Request-ID", xid.New().String())
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cli: http: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, decodeAppError(resp)
	}
	return resp, nil
}

// GetJSON issues a GET and decodes the JSON body into out.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeJSON(resp.Body, out)
}

// PostJSON issues a POST with a JSON body and decodes the response into out.
func (c *Client) PostJSON(ctx context.Context, path string, in, out any) error {
	buf, err := jsonMarshal(in)
	if err != nil {
		return err
	}
	resp, err := c.doWithContentType(ctx, http.MethodPost, path, bytes.NewReader(buf), "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return decodeJSON(resp.Body, out)
}

// PutJSON issues a PUT with a JSON body and decodes the response into out.
func (c *Client) PutJSON(ctx context.Context, path string, in, out any) error {
	buf, err := jsonMarshal(in)
	if err != nil {
		return err
	}
	resp, err := c.doWithContentType(ctx, http.MethodPut, path, bytes.NewReader(buf), "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return decodeJSON(resp.Body, out)
}

// Delete issues a DELETE and returns the response body for the caller
// to close.
func (c *Client) Delete(ctx context.Context, path string) (*http.Response, error) {
	return c.Do(ctx, http.MethodDelete, path, nil)
}

// PostForm issues a POST with URL-encoded form body and decodes JSON out.
func (c *Client) PostForm(ctx context.Context, path string, form url.Values, out any) error {
	body := form.Encode()
	resp, err := c.doWithContentType(ctx, http.MethodPost, path, strings.NewReader(body), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return decodeJSON(resp.Body, out)
}

// Raw returns the underlying HTTP response for callers that need to
// stream bodies (e.g. `ov read` for binary content). The caller must
// close resp.Body.
func (c *Client) Raw(ctx context.Context, method, path string) (*http.Response, error) {
	return c.Do(ctx, method, path, nil)
}

// --- helpers ---

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// decodeAppError reads resp.Body, closes it, and converts a JSON error
// envelope into *domain.AppError. If the body is not JSON or has no
// recognized envelope, a generic AppError with the HTTP status is
// returned.
func decodeAppError(resp *http.Response) error {
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, resp.StatusCode, fmt.Errorf("read error body: %w", err))
	}
	var env errorEnvelope
	if jsonErr := json.Unmarshal(data, &env); jsonErr == nil && env.Error.Code != "" {
		ae := domain.NewAppError(env.Error.Code, resp.StatusCode, env.Error.Message)
		if len(env.Error.Details) > 0 {
			ae.Details = env.Error.Details
		}
		return ae
	}
	// Fall back to a generic AppError wrapping the raw body.
	return domain.Wrap(domain.CodeInternalError, resp.StatusCode, errors.New(strings.TrimSpace(string(data))))
}

func decodeJSON(r io.Reader, out any) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("cli: decode json: %w", err)
	}
	return nil
}

func jsonMarshal(in any) ([]byte, error) {
	if in == nil {
		return nil, nil
	}
	if b, ok := in.([]byte); ok {
		return b, nil
	}
	buf, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("cli: marshal json: %w", err)
	}
	return buf, nil
}
