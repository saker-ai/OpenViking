// Package ovmount is the bot's HTTP client to openviking-server. It
// exposes a minimal surface the agent loop needs: list resources,
// read a resource, search, remember (add memory). The MCP endpoint
// is the canonical tool path; ovmount is the lower-level HTTP client
// used by the bot's own housekeeping (e.g. heartbeat auth, session
// sync) rather than agent tool calls.
//
// P11 ships an HTTP client interface and a stub-backed default. The
// real wiring against pkg/sdk (when P10 lands) replaces the
// implementation; the interface stays.
package ovmount

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// Client is the OpenViking server HTTP client. All methods are
// context-aware and safe for concurrent use.
type Client interface {
	// Health pings the server's /api/v1/system endpoint.
	Health(ctx context.Context) error
	// List returns the children of the resource at uri.
	List(ctx context.Context, id domain.Identifier, uri string) ([]domain.Resource, error)
	// Read returns the resource at uri (metadata + content).
	Read(ctx context.Context, id domain.Identifier, uri string) (*domain.Resource, error)
	// Search runs a semantic search query.
	Search(ctx context.Context, id domain.Identifier, query string, topN int) ([]domain.Resource, error)
	// Remember persists a memory entry.
	Remember(ctx context.Context, id domain.Identifier, content string, metadata map[string]any) error
}

// HTTPClient is the default Client implementation. It talks to the
// openviking-server REST API.
type HTTPClient struct {
	cfg     config.OvmountConfig
	baseURL string
	client  *http.Client
}

// New returns an HTTPClient.
func New(cfg config.OvmountConfig, serverURL string) *HTTPClient {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &HTTPClient{
		cfg:     cfg,
		baseURL: strings.TrimRight(serverURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

// Health implements Client.
func (c *HTTPClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/system", nil)
	if err != nil {
		return fmt.Errorf("ovmount: new request: %w", err)
	}
	c.setAuth(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("ovmount: health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ovmount: health: http %d", resp.StatusCode)
	}
	return nil
}

// List implements Client.
func (c *HTTPClient) List(ctx context.Context, id domain.Identifier, uri string) ([]domain.Resource, error) {
	url := fmt.Sprintf("%s/api/v1/resources?uri=%s", c.baseURL, uri)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ovmount: new request: %w", err)
	}
	c.setAuth(req)
	c.setIdentity(req, id)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ovmount: list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ovmount: list: http %d", resp.StatusCode)
	}
	var out []domain.Resource
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ovmount: decode: %w", err)
	}
	return out, nil
}

// Read implements Client.
func (c *HTTPClient) Read(ctx context.Context, id domain.Identifier, uri string) (*domain.Resource, error) {
	url := fmt.Sprintf("%s/api/v1/resources/%s", c.baseURL, uri)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ovmount: new request: %w", err)
	}
	c.setAuth(req)
	c.setIdentity(req, id)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ovmount: read: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ovmount: read: http %d", resp.StatusCode)
	}
	var out domain.Resource
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ovmount: decode: %w", err)
	}
	return &out, nil
}

// Search implements Client.
func (c *HTTPClient) Search(ctx context.Context, id domain.Identifier, query string, topN int) ([]domain.Resource, error) {
	url := fmt.Sprintf("%s/api/v1/search?q=%s&top_n=%d", c.baseURL, query, topN)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ovmount: new request: %w", err)
	}
	c.setAuth(req)
	c.setIdentity(req, id)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ovmount: search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ovmount: search: http %d", resp.StatusCode)
	}
	var out []domain.Resource
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ovmount: decode: %w", err)
	}
	return out, nil
}

// Remember implements Client.
func (c *HTTPClient) Remember(ctx context.Context, id domain.Identifier, content string, metadata map[string]any) error {
	body, _ := json.Marshal(map[string]any{
		"content":  content,
		"metadata": metadata,
	})
	url := c.baseURL + "/api/v1/memories"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ovmount: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	c.setIdentity(req, id)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("ovmount: remember: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ovmount: remember: http %d", resp.StatusCode)
	}
	return nil
}

func (c *HTTPClient) setAuth(req *http.Request) {
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
}

func (c *HTTPClient) setIdentity(req *http.Request, id domain.Identifier) {
	if id.Account != "" {
		req.Header.Set("X-OpenViking-Account", id.Account)
	}
	if id.User != "" {
		req.Header.Set("X-OpenViking-User", id.User)
	}
	if id.ActorPeer != "" {
		req.Header.Set("X-OpenViking-Actor-Peer", id.ActorPeer)
	}
}
