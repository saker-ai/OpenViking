// Package vlm — OpenAI-compatible chat + embeddings client.
//
// This file implements VLM over the public OpenAI HTTP API (/v1/chat/completions,
// /v1/embeddings). The same client shape is reused by every OpenAI-compatible
// gateway (Azure OpenAI, Volcengine Ark /api/v3, LiteLLM, local proxy) by
// overriding the BaseURL and Authorization header in NewOpenAI.
//
// The implementation deliberately uses net/http instead of openai/openai-go:
// the SDK's strongly-typed request/response shapes do not round-trip cleanly
// across providers (Azure requires "api-version", Volcengine differs on
// tool schemas), and a thin HTTP layer keeps the dependency surface minimal
// while remaining easy to test with httptest.NewServer.
//
// All network calls go through the injected Doer; tests inject a recorder
// and never hit the network.
package vlm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// OpenAIClient is a VLM backed by an OpenAI-compatible HTTP endpoint.
//
// BaseURL must include the trailing path up to but excluding /chat/completions
// (e.g. "https://api.openai.com/v1" or "https://ark.cn-beijing.volces.com/api/v3").
// APIKey is sent as a Bearer token unless AuthHeader is set, in which case
// the latter is used verbatim (e.g. "api-key: <key>" for Azure).
type OpenAIClient struct {
	BaseURL    string
	APIKey     string
	AuthHeader string // optional, overrides default "Authorization: Bearer <key>"
	Model      string
	HTTP       Doer
	Timeout    time.Duration
}

// NewOpenAI constructs an OpenAI-compatible VLM from a provider/model/api triple.
// httpCLI may be nil; the default is a 30s-timeout *http.Client.
func NewOpenAI(baseURL, apiKey, model string, httpCLI *http.Client) *OpenAIClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    httpCLI,
		Timeout: 30 * time.Second,
	}
}

// openAIChatRequest is the wire shape for POST /chat/completions.
type openAIChatRequest struct {
	Model          string          `json:"model"`
	Messages       []openAIMessage `json:"messages"`
	Tools          []openAITool    `json:"tools,omitempty"`
	ResponseFormat *openAIRespFmt  `json:"response_format,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

type openAIRespFmt struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
}

// Chat calls POST {BaseURL}/chat/completions.
func (c *OpenAIClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if c.Model != "" {
		if req.Model == "" {
			req.Model = c.Model
		}
	}
	if req.Model == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vlm.openai: model is required"))
	}
	body, err := json.Marshal(toOpenAIReq(req))
	if err != nil {
		return nil, wrapVLM(fmt.Errorf("encode request: %w", err))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, wrapVLM(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuth(httpReq)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, wrapVLM(fmt.Errorf("http: %w", err))
	}
	defer drainAndClose(resp.Body)
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var raw openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, wrapVLM(fmt.Errorf("decode response: %w", err))
	}
	if len(raw.Choices) == 0 {
		return nil, wrapVLM(fmt.Errorf("vlm.openai: empty choices"))
	}
	ch := raw.Choices[0]
	out := &ChatResponse{
		Model:        raw.Model,
		Content:      ch.Message.Content,
		FinishReason: ch.FinishReason,
		Usage: Usage{
			PromptTokens:     raw.Usage.PromptTokens,
			CompletionTokens: raw.Usage.CompletionTokens,
			TotalTokens:      raw.Usage.TotalTokens,
		},
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

// Embed calls POST {BaseURL}/embeddings with the OpenAI wire shape.
func (c *OpenAIClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	model := c.Model
	if model == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vlm.openai: model is required for embed"))
	}
	body, err := json.Marshal(map[string]any{
		"model": model,
		"input": texts,
	})
	if err != nil {
		return nil, wrapVLM(err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, wrapVLM(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuth(httpReq)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, wrapVLM(fmt.Errorf("http: %w", err))
	}
	defer drainAndClose(resp.Body)
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var raw struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, wrapVLM(fmt.Errorf("decode response: %w", err))
	}
	out := make([][]float32, len(texts))
	for i, d := range raw.Data {
		if i >= len(out) {
			break
		}
		out[i] = d.Embedding
	}
	return out, nil
}

func (c *OpenAIClient) setAuth(h *http.Request) {
	if c.AuthHeader != "" {
		parts := strings.SplitN(c.AuthHeader, ":", 2)
		if len(parts) == 2 {
			h.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
			return
		}
	}
	if c.APIKey != "" {
		h.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
}

func toOpenAIReq(req ChatRequest) openAIChatRequest {
	out := openAIChatRequest{
		Model:     req.Model,
		Stream:    req.Stream,
		Stop:      req.Stop,
		MaxTokens: nilIfZero(req.MaxTokens),
	}
	if req.Temperature > 0 {
		t := req.Temperature
		out.Temperature = &t
	}
	if req.TopP > 0 {
		p := req.TopP
		out.TopP = &p
	}
	for _, m := range req.Messages {
		om := openAIMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			otc := openAIToolCall{ID: tc.ID, Type: "function"}
			otc.Function.Name = tc.Name
			otc.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		out.Messages = append(out.Messages, om)
	}
	for _, t := range req.Tools {
		ot := openAITool{Type: "function"}
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Schema
		out.Tools = append(out.Tools, ot)
	}
	if req.ResponseFormat != nil {
		out.ResponseFormat = &openAIRespFmt{
			Type:       req.ResponseFormat.Type,
			JSONSchema: req.ResponseFormat.JSONSchema,
		}
	}
	return out
}

func nilIfZero[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

// checkStatus converts a non-2xx response into a domain-wrapped error.
func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return wrapVLM(fmt.Errorf("vlm: http %d: %s", resp.StatusCode, string(body)))
}

func drainAndClose(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 4096))
	_ = r.Close()
}
