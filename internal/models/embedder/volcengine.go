// Package embedder — Volcengine Ark (Doubao) embeddings client built on
// the official SDK.
//
// Ark exposes an OpenAI-compatible embeddings surface at
// https://ark.cn-beijing.volces.com/api/v3/embeddings — we delegate the
// wire format, auth header, and retry to
// github.com/volcengine/volcengine-go-sdk/service/arkruntime and only
// adapt the provider-agnostic Embedder interface onto the SDK's typed
// params. Doubao embedding endpoints are passed verbatim as "model".
//
// Tests inject an httptest.Server as the SDK's base URL + HTTP client;
// no network calls.
package embedder

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// arkInitMu serializes arkruntime client construction. The SDK's embedded
// ark session mutates http.DefaultClient.Transport and
// http.DefaultTransport.Proxy during NewClientWithApiKey; without a lock,
// concurrent constructions (e.g. parallel tests) trigger data races in the
// SDK's session package. The ark session is only used for STS token
// refresh, which the API-key auth path never invokes, so the lock is
// only contended at construction time — not on the hot request path.
var arkInitMu sync.Mutex

// DefaultVolcengineBaseURL is the Ark OpenAI-compatible endpoint.
const DefaultVolcengineBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

// VolcengineClient is an Embedder backed by the Volcengine Ark arkruntime
// SDK.
//
// BaseURL/APIKey/Model/HTTP are mirrored as struct fields so tests can
// assert against the configured values without poking at SDK internals.
// DimByModel overrides the dimension table for callers that ship a
// non-default dimension (e.g. Doubao truncation).
type VolcengineClient struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTP       Doer
	DimByModel map[string]int
	cli        *arkruntime.Client
}

// NewVolcengine constructs an Ark-backed embedder. httpCLI may be nil; the
// default is a 30s-timeout *http.Client. When cfg.Dim is non-zero, the
// resulting client's DimByModel table pins cfg.Model to cfg.Dim so
// Dimensions() returns the caller-specified value.
func NewVolcengine(cfg config.EmbedderConfig, httpCLI *http.Client) *VolcengineClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	base := cfg.APIBase
	if base == "" {
		base = DefaultVolcengineBaseURL
	}
	canonical := strings.TrimRight(base, "/")
	arkInitMu.Lock()
	cli := arkruntime.NewClientWithApiKey(
		cfg.APIKey,
		arkruntime.WithBaseUrl(canonical),
		arkruntime.WithHTTPClient(httpCLI),
		// Disable SDK retries so error tests see a single request and
		// succeed deterministically without spinning on backoff.
		arkruntime.WithRetryTimes(0),
	)
	arkInitMu.Unlock()
	c := &VolcengineClient{
		BaseURL: canonical,
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
		HTTP:    httpCLI,
		cli:     cli,
	}
	if cfg.Dim > 0 {
		c.DimByModel = map[string]int{cfg.Model: cfg.Dim}
	}
	return c
}

// Embed calls POST {BaseURL}/embeddings via the SDK. If model is empty,
// falls back to c.Model.
func (c *VolcengineClient) Embed(ctx context.Context, texts []string, modelID string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if modelID == "" {
		modelID = c.Model
	}
	if modelID == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("embedder.volcengine: model is required"))
	}
	resp, err := c.cli.CreateEmbeddings(ctx, model.EmbeddingRequestStrings{
		Input: texts,
		Model: modelID,
	})
	if err != nil {
		return nil, wrapEmbed(mapArkError(err))
	}
	out := make([][]float32, len(texts))
	for i, d := range resp.Data {
		if i >= len(out) {
			break
		}
		out[i] = d.Embedding
	}
	return out, nil
}

// Dimensions returns the dimensionality for the given model, using the
// DimByModel override table if present, otherwise knownDefaultDim.
func (c *VolcengineClient) Dimensions(model string) int {
	if c.DimByModel != nil {
		if d, ok := c.DimByModel[model]; ok && d > 0 {
			return d
		}
	}
	return knownDefaultDim(model)
}

// mapArkError inspects an arkruntime SDK error and returns a typed error
// that callers can match with errors.Is/errors.As against the SDK's
// model.APIError / model.RequestError. Non-SDK errors pass through
// unchanged so domain.Wrap can annotate them with the embed business code.
func mapArkError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *model.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("embedder.volcengine: http %d: %s",
			apiErr.HTTPStatusCode, apiErr.Error())
	}
	var reqErr *model.RequestError
	if errors.As(err, &reqErr) {
		return fmt.Errorf("embedder.volcengine: http %d: %v",
			reqErr.HTTPStatusCode, reqErr.Err)
	}
	return err
}
