// Package vlm — Volcengine Ark (Doubao) chat + embeddings client built on
// the official SDK.
//
// Ark exposes an OpenAI-compatible HTTP surface at
// https://ark.cn-beijing.volces.com/api/v3 — we delegate the wire format,
// auth header, retry, and SSE plumbing to
// github.com/volcengine/volcengine-go-sdk/service/arkruntime and only
// adapt the provider-agnostic ChatRequest/ChatResponse shapes onto the
// SDK's typed params. Doubao endpoints are passed verbatim as "model";
// callers pick the concrete endpoint id (e.g. "ep-20241234567-xxxxx")
// from the Ark console.
//
// Tests inject an httptest.Server as the SDK's base URL + HTTP client;
// no network calls.
package vlm

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

// DefaultVolcengineBaseURL is the Ark OpenAI-compatible endpoint. May be
// overridden via VLMConfig.APIBase.
const DefaultVolcengineBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

// VolcengineClient is a VLM backed by the Volcengine Ark arkruntime SDK.
//
// The embedded arkruntime.Client handles signing, JSON marshalling, and
// retry. BaseURL/APIKey/Model/HTTP are mirrored as struct fields so tests
// can assert against the configured values without poking at SDK internals.
type VolcengineClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
	cli     *arkruntime.Client
}

// NewVolcengine constructs an Ark-backed VLM from a VLMConfig. httpCLI may
// be nil; the default is a 30s-timeout *http.Client. The HTTP client and
// base URL are forwarded to the SDK via WithHTTPClient / WithBaseUrl so
// tests can substitute an httptest.Server recorder.
func NewVolcengine(cfg config.VLMConfig, httpCLI *http.Client) *VolcengineClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	base := cfg.APIBase
	if base == "" {
		base = DefaultVolcengineBaseURL
	}
	// WithBaseUrl trims a trailing slash; mirror that here so c.BaseURL
	// matches what the SDK will actually use.
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
	return &VolcengineClient{
		BaseURL: canonical,
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
		HTTP:    httpCLI,
		cli:     cli,
	}
}

// Chat calls POST {BaseURL}/chat/completions via the SDK.
func (c *VolcengineClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	modelID := req.Model
	if modelID == "" {
		modelID = c.Model
	}
	if modelID == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vlm.volcengine: model is required"))
	}

	sdkReq := model.ChatCompletionRequest{
		Model: modelID,
		Tools: toArkTools(req.Tools),
		Stop:  req.Stop,
	}
	if req.MaxTokens > 0 {
		sdkReq.MaxTokens = req.MaxTokens
	}
	if req.Temperature > 0 {
		sdkReq.Temperature = float32(req.Temperature)
	}
	if req.TopP > 0 {
		sdkReq.TopP = float32(req.TopP)
	}
	if req.Stream {
		sdkReq.Stream = true
	}
	sdkReq.Messages = toArkMessages(req.Messages)
	if req.ResponseFormat != nil {
		sdkReq.ResponseFormat = toArkResponseFormat(req.ResponseFormat)
	}

	resp, err := c.cli.CreateChatCompletion(ctx, sdkReq)
	if err != nil {
		return nil, wrapVLM(mapArkError(err))
	}
	if len(resp.Choices) == 0 {
		return nil, wrapVLM(fmt.Errorf("vlm.volcengine: empty choices"))
	}
	ch := resp.Choices[0]
	out := &ChatResponse{
		Model:        resp.Model,
		FinishReason: string(ch.FinishReason),
		Usage: Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
	if ch.Message.Content != nil && ch.Message.Content.StringValue != nil {
		out.Content = *ch.Message.Content.StringValue
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return out, nil
}

// Embed calls POST {BaseURL}/embeddings via the SDK. The model id is
// resolved from the request, falling back to c.Model.
func (c *VolcengineClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	modelID := c.Model
	if modelID == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vlm.volcengine: model is required for embed"))
	}
	resp, err := c.cli.CreateEmbeddings(ctx, model.EmbeddingRequestStrings{
		Input: texts,
		Model: modelID,
	})
	if err != nil {
		return nil, wrapVLM(mapArkError(err))
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

// toArkMessages maps the provider-agnostic Message slice onto the SDK's
// ChatCompletionMessage. Content is plain text (multimodal parts are out
// of scope for the first cut).
func toArkMessages(msgs []Message) []*model.ChatCompletionMessage {
	out := make([]*model.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		out = append(out, &model.ChatCompletionMessage{
			Role:    string(m.Role),
			Content: &model.ChatCompletionMessageContent{StringValue: &content},
			Name:    ptrIfNonEmpty(m.Name),
		})
	}
	return out
}

func toArkTools(tools []Tool) []*model.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]*model.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, &model.Tool{
			Type: model.ToolTypeFunction,
			Function: &model.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}
	return out
}

func toArkResponseFormat(rf *ResponseFormat) *model.ResponseFormat {
	if rf == nil {
		return nil
	}
	out := &model.ResponseFormat{
		Type: model.ResponseFormatType(rf.Type),
	}
	if rf.JSONSchema != nil {
		out.JSONSchema = &model.ResponseFormatJSONSchemaJSONSchemaParam{
			Name:        stringOr(rf.JSONSchema["name"], "schema"),
			Description: stringOr(rf.JSONSchema["description"], ""),
			Schema:      rf.JSONSchema["schema"],
		}
	}
	return out
}

func stringOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func ptrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// mapArkError inspects an arkruntime SDK error and returns a typed error
// that callers can match with errors.Is/errors.As against the SDK's
// model.APIError / model.RequestError. Non-SDK errors pass through
// unchanged so domain.Wrap can annotate them with the VLM business code.
func mapArkError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *model.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("vlm.volcengine: http %d: %s",
			apiErr.HTTPStatusCode, apiErr.Error())
	}
	var reqErr *model.RequestError
	if errors.As(err, &reqErr) {
		return fmt.Errorf("vlm.volcengine: http %d: %v",
			reqErr.HTTPStatusCode, reqErr.Err)
	}
	return err
}

// trimRight strips trailing '/' characters from s. Shared with dashscope.go.
func trimRight(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// orDefault returns c when non-nil, otherwise a fresh *http.Client. Shared
// with dashscope.go.
func orDefault(c *http.Client) Doer {
	if c == nil {
		return &http.Client{}
	}
	return c
}
