package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// newTestOpenAI builds an OpenAIProvider wired to a httptest server
// with retries disabled (the SDK defaults to 2 retries on 5xx, which
// would slow tests by ~1.5s per failure). The same-package test can
// poke the client field directly.
func newTestOpenAI(t *testing.T, cfg config.ProviderConfig, handler http.HandlerFunc) (*OpenAIProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	cfg.Backend = "openai"
	cfg.BaseURL = srv.URL
	p := NewOpenAI(cfg)
	// Replace the client with one that does not retry, so 5xx tests
	// return immediately and 2xx tests are not slowed by backoff.
	p.client = openai.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey(cfg.APIKey),
		option.WithMaxRetries(0),
	)
	return p, srv
}

func TestOpenAIProvider_ChatSuccess(t *testing.T) {
	var gotBody map[string]any
	p, _ := newTestOpenAI(t, config.ProviderConfig{
		APIKey: "sk-test",
		Model:  "gpt-4o-mini",
	}, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		// Echo a tool call back.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": 0,
			"model":   "gpt-4o-mini",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "find",
							"arguments": `{"q":"x"}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
		})
	})

	resp, err := p.Chat(context.Background(), []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "find x"},
	}, []ToolDescription{{Name: "find", Description: "find a resource"}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "find" || tc.Args["q"] != "x" {
		t.Errorf("ToolCall = %+v", tc)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if got, _ := gotBody["model"].(string); got != "gpt-4o-mini" {
		t.Errorf("request model = %v, want gpt-4o-mini", got)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Errorf("request messages len = %d, want 2", len(msgs))
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("request tools len = %d, want 1", len(tools))
	} else {
		fn := tools[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "find" {
			t.Errorf("request tool name = %v, want find", fn["name"])
		}
		if fn["description"] != "find a resource" {
			t.Errorf("request tool description = %v", fn["description"])
		}
	}
}

func TestOpenAIProvider_ChatContentOnly(t *testing.T) {
	// Verifies a plain text response with no tool calls maps through.
	p, _ := newTestOpenAI(t, config.ProviderConfig{
		APIKey: "sk-test",
		Model:  "gpt-4o-mini",
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": 0,
			"model":   "gpt-4o-mini",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "hello there",
				},
				"finish_reason": "stop",
			}},
		})
	})
	resp, err := p.Chat(context.Background(), []Message{
		{Role: RoleUser, Content: "hi"},
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hello there" {
		t.Errorf("Content = %q, want hello there", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %+v, want empty", resp.ToolCalls)
	}
}

func TestOpenAIProvider_ChatEmptyChoices(t *testing.T) {
	p, _ := newTestOpenAI(t, config.ProviderConfig{
		APIKey: "sk-test",
		Model:  "gpt-4o-mini",
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": 0,
			"model":   "gpt-4o-mini",
			"choices": []any{},
		})
	})
	_, err := p.Chat(context.Background(), []Message{
		{Role: RoleUser, Content: "hi"},
	}, nil)
	if err != ErrEmptyResponse {
		t.Fatalf("err = %v, want ErrEmptyResponse", err)
	}
}

func TestOpenAIProvider_ChatHTTPError(t *testing.T) {
	p, _ := newTestOpenAI(t, config.ProviderConfig{
		APIKey: "sk-test",
		Model:  "gpt-4o-mini",
	}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want 500", err)
	}
}

func TestOpenAIProvider_ChatEmptyBaseURL(t *testing.T) {
	p := NewOpenAI(config.ProviderConfig{Backend: "openai"})
	_, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "base_url is empty") {
		t.Fatalf("err = %v, want base_url is empty", err)
	}
}
