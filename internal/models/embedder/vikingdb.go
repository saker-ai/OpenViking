// Package embedder — VikingDB embeddings client.
//
// Implements the VikingDB /api/vikingdb/embedding HTTP API via the
// volc-sdk-golang/service/vikingdb SDK. The SDK ships an internal V4
// HMAC-SHA256 signer (service component "air"), so this client delegates
// all signing and HTTP transport to *base.Client. Tests inject a custom
// *http.Client via WithHTTPClient to point at httptest.NewServer.
//
// VikingDB is Volcengine's managed vector DB service. The request shape
// is {dense_model: {name, version, dim}, data: [{text, input_type?}]}
// and the response shape is {result: {data: [{dense_embedding: [float]}]}}.
//
// Tests inject an httptest.Server via WithHTTPClient; no network calls.
package embedder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/volcengine/volc-sdk-golang/base"
	vikingdb "github.com/volcengine/volc-sdk-golang/service/vikingdb"
)

// DefaultVikingDBHost is the canonical VikingDB API host (cn-beijing).
const DefaultVikingDBHost = "https://api.vikingdb-cn-beijing.volces.com"

// vikingDBServiceName is the V4 credential-scope service component for
// VikingDB. The reference SDK uses "air", not "vikingdb".
const vikingDBServiceName = "air"

// VikingDBClient is an Embedder backed by VikingDB /api/vikingdb/embedding.
//
// The SDK signs every request with Volcengine AK/SK HMAC and sends it via
// the embedded *base.Client. Production code passes the real credentials;
// tests pass an httptest.Server URL and inject srv.Client() via
// WithHTTPClient so signed requests still hit the test server.
type VikingDBClient struct {
	svc *vikingdb.VikingDBService

	// Host is the resolved host (no scheme) retained for diagnostics.
	Host string
	// AK/SK are Volcengine credentials.
	AK    string
	SK    string
	Model string
	Version string
	// Dim is the configured dense dimension. Default 2048.
	Dim int
}

// NewVikingDB constructs a VikingDB-backed embedder. httpCLI may be nil;
// when non-nil it overrides the SDK's default *http.Client (used by tests
// to point at an httptest.Server).
//
// model is the VikingDB embedding model name (e.g. "bge-large-zh").
// version is the model version (may be empty). dim, when > 0, overrides
// the default 2048.
func NewVikingDB(host, ak, sk, model, version string, dim int, httpCLI *http.Client) *VikingDBClient {
	if host == "" {
		host = DefaultVikingDBHost
	}
	region := "cn-beijing"
	scheme := "https"
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		if u.Scheme != "" {
			scheme = u.Scheme
		}
		host = u.Host
	}
	info := &base.ServiceInfo{
		Timeout: 30 * time.Second, Scheme: scheme, Host: host,
		Header: http.Header{"Host": []string{host}},
		Credentials: base.Credentials{
			AccessKeyID:     ak,
			SecretAccessKey: sk,
			Service:         vikingDBServiceName,
			Region:          region,
		},
	}
	svc := &vikingdb.VikingDBService{Client: base.NewClient(info, vikingDBEmbeddingAPIInfo())}
	if httpCLI != nil {
		svc.Client.Client = httpCLI
	}
	return &VikingDBClient{
		svc:     svc,
		Host:    host,
		AK:      ak,
		SK:      sk,
		Model:   model,
		Version: version,
		Dim:     dim,
	}
}

// WithHTTPClient replaces the SDK client's *http.Client. Used by tests to
// route signed requests through an httptest.Server's client.
func (c *VikingDBClient) WithHTTPClient(cli *http.Client) *VikingDBClient {
	if cli != nil && c.svc != nil && c.svc.Client != nil {
		c.svc.Client.Client = cli
	}
	return c
}

// vikingDBEmbeddingAPIInfo returns the API info table for the embedding
// endpoint. We register "Embedding" against /api/vikingdb/embedding
// (the embedder's data-plane path), overriding the SDK's default
// /api/data/embedding entry — the two are distinct endpoints.
func vikingDBEmbeddingAPIInfo() map[string]*base.ApiInfo {
	h := http.Header{"Accept": []string{"application/json"}, "Content-Type": []string{"application/json"}}
	return map[string]*base.ApiInfo{
		"Embedding": {Method: http.MethodPost, Path: "/api/vikingdb/embedding", Header: h},
	}
}

// vikingDBRequest is the /api/vikingdb/embedding request shape.
type vikingDBRequest struct {
	DenseModel vikingDBModel  `json:"dense_model"`
	Data       []vikingDBItem `json:"data"`
}

type vikingDBModel struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Dim     int    `json:"dim,omitempty"`
}

type vikingDBItem struct {
	Text      string `json:"text"`
	InputType string `json:"input_type,omitempty"`
}

// Embed calls POST {Host}/api/vikingdb/embedding via the SDK signer.
func (c *VikingDBClient) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	m := model
	if m == "" {
		m = c.Model
	}
	if m == "" {
		return nil, fmt.Errorf("embedder.vikingdb: model is required")
	}
	items := make([]vikingDBItem, len(texts))
	for i, t := range texts {
		items[i] = vikingDBItem{Text: t}
	}
	body, err := json.Marshal(vikingDBRequest{
		DenseModel: vikingDBModel{Name: m, Version: c.Version, Dim: c.Dim},
		Data:       items,
	})
	if err != nil {
		return nil, wrapEmbed(fmt.Errorf("encode request: %w", err))
	}
	if c.svc == nil {
		return nil, wrapEmbed(errors.New("embedder.vikingdb: nil vikingdb service"))
	}
	res, err := c.svc.DoRequest(ctx, "Embedding", nil, string(body))
	if err != nil {
		return nil, wrapEmbed(fmt.Errorf("embedder.vikingdb: %w", err))
	}
	out, err := parseVikingDBEmbeddingResponse(res)
	if err != nil {
		return nil, wrapEmbed(err)
	}
	return out, nil
}

// parseVikingDBEmbeddingResponse navigates the SDK-decoded JSON map and
// returns the dense_embedding vectors in input order.
func parseVikingDBEmbeddingResponse(res map[string]any) ([][]float32, error) {
	if res == nil {
		return nil, errors.New("embedder.vikingdb: empty response")
	}
	result, _ := res["result"].(map[string]any)
	if result == nil {
		return nil, fmt.Errorf("embedder.vikingdb: missing result in response: %v", res)
	}
	data, _ := result["data"].([]any)
	out := make([][]float32, 0, len(data))
	for _, item := range data {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		raw, _ := m["dense_embedding"].([]any)
		vec := make([]float32, 0, len(raw))
		for _, v := range raw {
			f, err := vikingDBToFloat32(v)
			if err != nil {
				return nil, fmt.Errorf("embedder.vikingdb: decode embedding: %w", err)
			}
			vec = append(vec, f)
		}
		out = append(out, vec)
	}
	return out, nil
}

// vikingDBToFloat32 converts a JSON-decoded number to float32.
func vikingDBToFloat32(v any) (float32, error) {
	switch n := v.(type) {
	case float64:
		return float32(n), nil
	case float32:
		return n, nil
	case int:
		return float32(n), nil
	case int64:
		return float32(n), nil
	case json.Number:
		f, err := n.Float64()
		return float32(f), err
	}
	return 0, fmt.Errorf("embedder.vikingdb: not a number: %T", v)
}

// Dimensions returns the configured dim, defaulting to 2048 (VikingDB
// dense embedding default).
func (c *VikingDBClient) Dimensions(model string) int {
	if c.Dim > 0 {
		return c.Dim
	}
	return 2048
}

// Compile-time assertion that VikingDBClient satisfies Embedder.
var _ Embedder = (*VikingDBClient)(nil)
