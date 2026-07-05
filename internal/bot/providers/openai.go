package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// OpenAIProvider is a Provider that talks to an OpenAI-compatible
// /chat/completions endpoint via the official openai-go SDK. Any
// OpenAI-compatible API (Azure OpenAI, Volcengine Ark, LiteLLM, local
// proxies) can be targeted by setting BaseURL.
type OpenAIProvider struct {
	cfg    config.ProviderConfig
	client openai.Client
}

// NewOpenAI returns an OpenAIProvider. The SDK client is constructed
// with the configured BaseURL and APIKey; when BaseURL is empty the
// SDK falls back to its default (https://api.openai.com/v1), but Chat
// still rejects the call so misconfiguration fails loudly.
func NewOpenAI(cfg config.ProviderConfig) *OpenAIProvider {
	opts := []option.RequestOption{
		option.WithHTTPClient(&http.Client{Timeout: 300 * time.Second}),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	return &OpenAIProvider{
		cfg:    cfg,
		client: openai.NewClient(opts...),
	}
}

// Chat implements Provider by calling the SDK's Chat.Completions.New.
// Tools are forwarded as function declarations; the response's first
// choice is mapped back to providers.Response (Content, ToolCalls,
// FinishReason).
func (p *OpenAIProvider) Chat(ctx context.Context, messages []Message, tools []ToolDescription) (*Response, error) {
	if p.cfg.BaseURL == "" {
		return nil, fmt.Errorf("providers: openai base_url is empty")
	}
	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(p.cfg.Model),
		Messages: toOpenAIMessages(messages),
		Tools:    toOpenAITools(tools),
	}
	cr, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("providers: openai chat: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, ErrEmptyResponse
	}
	choice := cr.Choices[0]
	out := &Response{
		Content:      choice.Message.Content,
		ToolCalls:    fromOpenAIToolCalls(choice.Message.ToolCalls),
		FinishReason: choice.FinishReason,
	}
	return out, nil
}

// toOpenAIMessages maps the provider-agnostic Message slice to the
// SDK's tagged-union ChatCompletionMessageParamUnion. Assistant turns
// that carry ToolCalls are forwarded so multi-round tool use works.
func toOpenAIMessages(msgs []Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			out = append(out, openai.ChatCompletionMessageParamUnion{
				OfSystem: &openai.ChatCompletionSystemMessageParam{
					Content: openai.ChatCompletionSystemMessageParamContentUnion{
						OfString: openai.String(m.Content),
					},
				},
			})
		case RoleUser:
			out = append(out, openai.ChatCompletionMessageParamUnion{
				OfUser: &openai.ChatCompletionUserMessageParam{
					Content: openai.ChatCompletionUserMessageParamContentUnion{
						OfString: openai.String(m.Content),
					},
				},
			})
		case RoleAssistant:
			asst := &openai.ChatCompletionAssistantMessageParam{}
			if m.Content != "" {
				asst.Content.OfString = openai.String(m.Content)
			}
			if len(m.ToolCalls) > 0 {
				asst.ToolCalls = make([]openai.ChatCompletionMessageToolCallParam, 0, len(m.ToolCalls))
				for _, c := range m.ToolCalls {
					args, _ := json.Marshal(c.Args)
					asst.ToolCalls = append(asst.ToolCalls, openai.ChatCompletionMessageToolCallParam{
						ID: c.ID,
						Function: openai.ChatCompletionMessageToolCallFunctionParam{
							Name:      c.Name,
							Arguments: string(args),
						},
					})
				}
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: asst})
		case RoleTool:
			out = append(out, openai.ChatCompletionMessageParamUnion{
				OfTool: &openai.ChatCompletionToolMessageParam{
					Content: openai.ChatCompletionToolMessageParamContentUnion{
						OfString: openai.String(m.Content),
					},
					ToolCallID: m.ToolCallID,
				},
			})
		default:
			// Unknown role: treat as user to avoid dropping the turn.
			out = append(out, openai.ChatCompletionMessageParamUnion{
				OfUser: &openai.ChatCompletionUserMessageParam{
					Content: openai.ChatCompletionUserMessageParamContentUnion{
						OfString: openai.String(m.Content),
					},
				},
			})
		}
	}
	return out
}

// toOpenAITools maps ToolDescription to the SDK's function tool param.
// Returns nil when no tools are configured so the request omits the
// `tools` field entirely (some compatible endpoints reject empty
// arrays).
func toOpenAITools(tools []ToolDescription) []openai.ChatCompletionToolParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		fn := openai.ChatCompletionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name:       t.Name,
				Parameters: openai.FunctionParameters(t.Parameters),
			},
		}
		if t.Description != "" {
			fn.Function.Description = openai.String(t.Description)
		}
		out = append(out, fn)
	}
	return out
}

// fromOpenAIToolCalls maps the SDK's response tool calls back to the
// provider-agnostic ToolCall. Arguments is a JSON string from the
// model; we best-effort decode it into a map (the agent loop will
// surface a tool error if the args don't validate).
func fromOpenAIToolCalls(calls []openai.ChatCompletionMessageToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		args := map[string]any{}
		if c.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(c.Function.Arguments), &args)
		}
		out = append(out, ToolCall{
			ID:   c.ID,
			Name: c.Function.Name,
			Args: args,
		})
	}
	return out
}
