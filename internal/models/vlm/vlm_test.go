package vlm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func vlmConfig(model, key string) config.VLMConfig {
	return config.VLMConfig{Model: model, APIKey: key}
}

// anthropicWireReq mirrors the wire shape that the Anthropic SDK emits
// for POST /v1/messages. Tests decode incoming request bodies into this
// struct to assert that the SDK forwarded our ChatRequest correctly.
// The SDK sends system + content as arrays of typed blocks rather than
// bare strings, so we cannot reuse the old anthropicReq struct.
type anthropicWireReq struct {
	Model     string                  `json:"model"`
	MaxTokens int64                   `json:"max_tokens"`
	System    []anthropicWireBlock    `json:"system,omitempty"`
	Messages  []anthropicWireMessage  `json:"messages"`
	Tools     []anthropicWireTool     `json:"tools,omitempty"`
	Stop      []string                `json:"stop_sequences,omitempty"`
}

type anthropicWireBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicWireMessage struct {
	Role    string             `json:"role"`
	Content []anthropicWireBlock `json:"content"`
}

type anthropicWireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

func TestStubChatUnsupportedByDefault(t *testing.T) {
	t.Parallel()
	s := NewStub()
	_, err := s.Chat(context.Background(), ChatRequest{Model: "x"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUnsupported))
}

func TestStubChatRecordsCalls(t *testing.T) {
	t.Parallel()
	s := NewStub()
	s.ChatFn = func(_ context.Context, _ ChatRequest) (*ChatResponse, error) {
		return &ChatResponse{Content: "ok"}, nil
	}
	resp, err := s.Chat(context.Background(), ChatRequest{Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	require.Len(t, s.Calls, 1)
	assert.Equal(t, "chat", s.Calls[0].Kind)
	assert.Equal(t, "gpt-4o", s.Calls[0].Chat.Model)
}

func TestStubReset(t *testing.T) {
	t.Parallel()
	s := NewStub()
	s.ChatFn = func(_ context.Context, _ ChatRequest) (*ChatResponse, error) { return nil, nil }
	_, _ = s.Chat(context.Background(), ChatRequest{})
	require.Len(t, s.Calls, 1)
	s.Reset()
	assert.Empty(t, s.Calls)
}

func TestOpenAIChatSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		var in openAIChatRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Equal(t, "gpt-4o", in.Model)
		require.Len(t, in.Messages, 1)
		assert.Equal(t, "user", in.Messages[0].Role)
		assert.Equal(t, "hi", in.Messages[0].Content)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "gpt-4o",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "hello there",
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     5,
				"completion_tokens": 3,
				"total_tokens":      8,
			},
		})
	}))
	defer srv.Close()

	c := NewOpenAI(srv.URL+"/v1", "sk-test", "gpt-4o", srv.Client())
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello there", resp.Content)
	assert.Equal(t, "stop", resp.FinishReason)
	assert.Equal(t, 8, resp.Usage.TotalTokens)
}

func TestOpenAIChatModelFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in openAIChatRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		// Caller did not set Model; client falls back to its configured Model.
		assert.Equal(t, "default-model", in.Model)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": ""}, "finish_reason": "stop"}},
		})
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL+"/v1", "sk-test", "default-model", srv.Client())
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	require.NoError(t, err)
}

func TestOpenAIChatErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "rate limit")
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL+"/v1", "sk-test", "gpt-4o", srv.Client())
	_, err := c.Chat(context.Background(), ChatRequest{Model: "gpt-4o"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrVLMFailed))
	assert.Contains(t, err.Error(), "429")
}

func TestOpenAIChatMissingModel(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "", "", nil)
	_, err := c.Chat(context.Background(), ChatRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation))
}

func TestOpenAIEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/embeddings", r.URL.Path)
		var in map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "text-embedding-3-small", in["model"])
		assert.Equal(t, []any{"a", "b"}, in["input"])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}},
				{"embedding": []float32{0.4, 0.5, 0.6}},
			},
		})
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL+"/v1", "sk-test", "text-embedding-3-small", srv.Client())
	got, err := c.Embed(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, got[0])
	assert.Equal(t, []float32{0.4, 0.5, 0.6}, got[1])
}

func TestOpenAIEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "k", "m", nil)
	got, err := c.Embed(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestOpenAIEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "", "", nil)
	_, err := c.Embed(context.Background(), []string{"a"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation))
}

func TestAnthropicChatSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		require.Equal(t, "sk-ant-test", r.Header.Get("x-api-key"))
		require.Equal(t, anthropicVersion, r.Header.Get("anthropic-version"))
		var in anthropicWireReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "claude-3-5-sonnet-20241022", in.Model)
		require.Len(t, in.System, 1)
		assert.Equal(t, "you are helpful", in.System[0].Text)
		require.Len(t, in.Messages, 1)
		assert.Equal(t, "user", in.Messages[0].Role)
		require.Len(t, in.Messages[0].Content, 1)
		assert.Equal(t, "hi", in.Messages[0].Content[0].Text)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "claude-3-5-sonnet-20241022",
			"content": []map[string]any{
				{"type": "text", "text": "hello back"},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  4,
				"output_tokens": 3,
			},
		})
	}))
	defer srv.Close()
	c := NewAnthropic(srv.URL, "sk-ant-test", "claude-3-5-sonnet-20241022", srv.Client())
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:     "claude-3-5-sonnet-20241022",
		MaxTokens: 256,
		Messages: []Message{
			{Role: RoleSystem, Content: "you are helpful"},
			{Role: RoleUser, Content: "hi"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello back", resp.Content)
	assert.Equal(t, "stop", resp.FinishReason)
	assert.Equal(t, 7, resp.Usage.TotalTokens)
}

func TestAnthropicChatDefaultsMaxTokens(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in anthropicWireReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, int64(1024), in.MaxTokens) // default applied
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": ""}},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()
	c := NewAnthropic(srv.URL, "k", "claude", srv.Client())
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	require.NoError(t, err)
}

func TestAnthropicEmbedUnsupported(t *testing.T) {
	t.Parallel()
	c := NewAnthropic("", "k", "claude", nil)
	_, err := c.Embed(context.Background(), []string{"a"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUnsupported))
}

func TestAnthropicChatErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewAnthropic(srv.URL, "bad", "claude", srv.Client())
	_, err := c.Chat(context.Background(), ChatRequest{Model: "claude", MaxTokens: 8})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrVLMFailed))
}

func TestVolcengineClientDefaultsBaseURL(t *testing.T) {
	t.Parallel()
	c := NewVolcengine(vlmConfig("doubao-1-5-pro", "sk-volc"), nil)
	assert.Equal(t, DefaultVolcengineBaseURL, c.BaseURL)
	assert.Equal(t, "doubao-1-5-pro", c.Model)
	assert.Equal(t, "sk-volc", c.APIKey)
}

func TestVolcengineClientHonorsAPIBase(t *testing.T) {
	t.Parallel()
	cfg := vlmConfig("m", "k")
	cfg.APIBase = "https://custom.example/api/v3/"
	c := NewVolcengine(cfg, nil)
	assert.Equal(t, "https://custom.example/api/v3", c.BaseURL)
}

// volcengineWireReq mirrors the wire shape that the arkruntime SDK emits
// for POST /api/v3/chat/completions. Tests decode incoming request bodies
// into this struct to assert that the SDK forwarded our ChatRequest
// correctly. The SDK sends content as a bare string (not an array of
// typed blocks) when ChatCompletionMessageContent.StringValue is set.
type volcengineWireReq struct {
	Model       string                   `json:"model"`
	Messages    []volcengineWireMessage  `json:"messages"`
	Tools       []volcengineWireTool     `json:"tools,omitempty"`
	MaxTokens   int                      `json:"max_tokens,omitempty"`
	Temperature float32                  `json:"temperature,omitempty"`
	Stream      bool                     `json:"stream,omitempty"`
}

type volcengineWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type volcengineWireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

func TestVolcengineChatSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer sk-volc", r.Header.Get("Authorization"))
		var in volcengineWireReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "doubao-1-5-pro", in.Model)
		require.Len(t, in.Messages, 1)
		assert.Equal(t, "user", in.Messages[0].Role)
		assert.Equal(t, "hi", in.Messages[0].Content)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "doubao-1-5-pro",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "hello there",
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     5,
				"completion_tokens": 3,
				"total_tokens":      8,
			},
		})
	}))
	defer srv.Close()
	c := NewVolcengine(config.VLMConfig{
		Provider: "volcengine",
		Model:    "doubao-1-5-pro",
		APIKey:   "sk-volc",
		APIBase:  srv.URL + "/api/v3",
	}, srv.Client())
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:    "doubao-1-5-pro",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello there", resp.Content)
	assert.Equal(t, "stop", resp.FinishReason)
	assert.Equal(t, 8, resp.Usage.TotalTokens)
}

func TestVolcengineChatModelFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in volcengineWireReq
		_ = json.NewDecoder(r.Body).Decode(&in)
		// Caller did not set Model; client falls back to its configured Model.
		assert.Equal(t, "default-ep", in.Model)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": ""},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	c := NewVolcengine(config.VLMConfig{
		Provider: "volcengine",
		Model:    "default-ep",
		APIKey:   "sk-volc",
		APIBase:  srv.URL + "/api/v3",
	}, srv.Client())
	_, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
}

func TestVolcengineChatErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "rate limit")
	}))
	defer srv.Close()
	c := NewVolcengine(config.VLMConfig{
		Provider: "volcengine",
		Model:    "doubao-1-5-pro",
		APIKey:   "sk-volc",
		APIBase:  srv.URL + "/api/v3",
	}, srv.Client())
	_, err := c.Chat(context.Background(), ChatRequest{Model: "doubao-1-5-pro"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrVLMFailed))
	assert.Contains(t, err.Error(), "429")
}

func TestVolcengineChatMissingModel(t *testing.T) {
	t.Parallel()
	c := NewVolcengine(config.VLMConfig{Provider: "volcengine", APIKey: "k"}, nil)
	_, err := c.Chat(context.Background(), ChatRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation))
}

func TestVolcengineChatToolCall(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in volcengineWireReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Len(t, in.Tools, 1)
		assert.Equal(t, "function", in.Tools[0].Type)
		assert.Equal(t, "get_weather", in.Tools[0].Function.Name)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "doubao-pro",
			"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "get_weather",
							"arguments": `{"city":"sf"}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
		})
	}))
	defer srv.Close()
	c := NewVolcengine(config.VLMConfig{
		Provider: "volcengine",
		Model:    "doubao-pro",
		APIKey:   "sk-volc",
		APIBase:  srv.URL + "/api/v3",
	}, srv.Client())
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model: "doubao-pro",
		Tools: []Tool{{
			Name:        "get_weather",
			Description: "Get weather",
			Schema:      map[string]any{"type": "object"},
		}},
	})
	require.NoError(t, err)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "call_1", resp.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", resp.ToolCalls[0].Name)
	assert.Equal(t, `{"city":"sf"}`, resp.ToolCalls[0].Arguments)
	assert.Equal(t, "tool_calls", resp.FinishReason)
}

func TestVolcengineEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/embeddings", r.URL.Path)
		require.Equal(t, "Bearer sk-volc", r.Header.Get("Authorization"))
		var in map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "doubao-embedding", in["model"])
		assert.Equal(t, []any{"a", "b"}, in["input"])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}},
				{"embedding": []float32{0.4, 0.5, 0.6}},
			},
		})
	}))
	defer srv.Close()
	c := NewVolcengine(config.VLMConfig{
		Provider: "volcengine",
		Model:    "doubao-embedding",
		APIKey:   "sk-volc",
		APIBase:  srv.URL + "/api/v3",
	}, srv.Client())
	got, err := c.Embed(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, got[0])
	assert.Equal(t, []float32{0.4, 0.5, 0.6}, got[1])
}

func TestVolcengineEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewVolcengine(config.VLMConfig{Provider: "volcengine", Model: "m", APIKey: "k"}, nil)
	got, err := c.Embed(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestVolcengineEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewVolcengine(config.VLMConfig{Provider: "volcengine", APIKey: "k"}, nil)
	_, err := c.Embed(context.Background(), []string{"a"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation))
}

func TestNilIfZeroInt(t *testing.T) {
	t.Parallel()
	p := nilIfZero(0)
	assert.Nil(t, p)
	p = nilIfZero(42)
	require.NotNil(t, p)
	assert.Equal(t, 42, *p)
}

func TestMapAnthropicStop(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"end_turn":      "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"stop_sequence": "stop",
		"weird":         "weird",
	}
	for in, want := range cases {
		assert.Equal(t, want, mapAnthropicStop(in))
	}
}

func TestToOpenAIReqOmitsZeroFields(t *testing.T) {
	t.Parallel()
	r := toOpenAIReq(ChatRequest{Model: "m"})
	assert.Nil(t, r.MaxTokens)
	assert.Nil(t, r.Temperature)
	assert.Nil(t, r.TopP)
	assert.Empty(t, r.Tools)
	assert.Empty(t, r.Messages)
}

func TestCheckStatus2xx(t *testing.T) {
	t.Parallel()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}
	assert.NoError(t, checkStatus(resp))
}
