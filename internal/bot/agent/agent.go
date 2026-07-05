// Package agent is the vikingbot agent loop. It bridges the channel
// adapters (which produce IncomingMessages) and the OpenViking MCP
// server (which exposes the find/search/read/list/remember/... tools).
//
// Loop shape (per design doc 7.11.1):
//
//  1. user message -> provider (with system prompt + MCP tool list)
//  2. provider returns text OR tool calls
//  3. on tool calls: invoke MCP tools, append results, repeat (up to
//     max_iterations)
//  4. on text: reply via the originating channel
//
// We use a hand-rolled loop rather than cloudwego/eino's graph agent
// because the loop is a straight forward "message -> tool call ->
// response" pattern; pulling in eino would add a heavy dep tree for
// no functional gain. The choice is documented here per the P11 task
// brief: "If cloudwego/eino is hard to install, fall back to a simpler
// hand-rolled agent loop with a system prompt + tool-call JSON
// schema. Document the choice in a comment."
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/saker-ai/ctxhub/internal/bot/channels"
	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/bot/observability"
	"github.com/saker-ai/ctxhub/internal/bot/providers"
	"github.com/saker-ai/ctxhub/internal/bot/session"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/privacy"
)

// MCPClient is the subset of the mark3labs/mcp-go client the agent
// loop uses. The real implementation wraps
// github.com/mark3labs/mcp-go/client; tests inject a stub.
type MCPClient interface {
	// ListTools returns the tools exposed by the MCP server.
	ListTools(ctx context.Context) ([]providers.ToolDescription, error)
	// CallTool invokes the named tool with args and returns the
	// result text. The MCP SDK returns a CallToolResult with one or
	// more content blocks; we concatenate the text blocks here.
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
}

// Agent is the vikingbot agent loop. It is safe for concurrent use.
type Agent struct {
	cfg      config.ProviderConfig
	provider providers.Provider
	mcp      MCPClient
	sessions *session.Adapter
	obs      *observability.Bot
	mu       sync.Mutex
}

// New constructs an Agent. The mcp argument may be nil — in that case
// the agent operates in "chat-only" mode (no tool calls).
func New(cfg config.ProviderConfig, p providers.Provider, mcp MCPClient, sess *session.Adapter, obs *observability.Bot) *Agent {
	return &Agent{
		cfg:      cfg,
		provider: p,
		mcp:      mcp,
		sessions: sess,
		obs:      obs,
	}
}

// Handle processes an inbound message and returns the reply text. It
// is the entry point called by the channel dispatcher.
//
// The loop:
//  1. Ensure / fetch the session for this (account, user, peer).
//  2. Redact PII from the user message (per-request PIIMap).
//  3. Append the redacted user turn (session store stays PII-free).
//  4. Build the conversation: system prompt + session turns + new
//     redacted user message.
//  5. Call the provider. On tool_calls, invoke each via MCP, append
//     tool-result turns, and repeat (up to cfg.MaxIterations).
//  6. Append the redacted assistant turn and return the restored reply.
func (a *Agent) Handle(ctx context.Context, msg channels.IncomingMessage) (string, error) {
	if a.provider == nil {
		return "", errors.New("agent: provider is nil")
	}
	if msg.Text == "" {
		return "", nil
	}

	// Span the whole agent turn so the bot's traces line up with
	// server-side spans (the openviking-server receives the
	// X-OpenViking-* headers via ovmount).
	if a.obs != nil {
		var end func(error)
		ctx, end = a.obs.Span(ctx, "agent.Handle")
		defer end(nil)
	}

	// Redact PII from the user message before it enters the session
	// store or the LLM prompt. The PIIMap is scoped to this Handle()
	// call so the LLM response can be restored before returning to
	// the user. The session persists the redacted form so PII does
	// not leak into long-term storage (per the privacy pipeline
	// design).
	redactedText, piiMap := privacy.Redact(msg.Text)
	redactedMsg := msg
	redactedMsg.Text = redactedText

	sessionID, err := a.sessions.EnsureSession(ctx, redactedMsg)
	if err != nil {
		return "", fmt.Errorf("agent: ensure session: %w", err)
	}
	if err := a.sessions.AppendUserTurn(ctx, redactedMsg, sessionID); err != nil {
		return "", fmt.Errorf("agent: append user turn: %w", err)
	}

	// Build the conversation history.
	conversation := a.buildConversation(ctx, redactedMsg, sessionID)

	// Fetch MCP tools (when available).
	var tools []providers.ToolDescription
	if a.mcp != nil {
		tools, err = a.mcp.ListTools(ctx)
		if err != nil {
			// Non-fatal: continue with no tools.
			tools = nil
		}
	}

	// Run the agent loop.
	maxIter := a.cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = 1
	}
	var reply string
	for iter := 0; iter < maxIter; iter++ {
		resp, err := a.provider.Chat(ctx, conversation, tools)
		if err != nil {
			return "", fmt.Errorf("agent: provider chat: %w", err)
		}
		if len(resp.ToolCalls) == 0 {
			reply = resp.Content
			break
		}
		// Append the assistant turn with tool calls, then dispatch
		// each tool and append the results.
		var toolResults []domain.ToolCall
		for _, tc := range resp.ToolCalls {
			result, callErr := a.callTool(ctx, tc.Name, tc.Args)
			if callErr != nil {
				result = "error: " + callErr.Error()
			}
			toolResults = append(toolResults, domain.ToolCall{
				ID:     tc.ID,
				Name:   tc.Name,
				Args:   tc.Args,
				Result: result,
			})
			// Append the tool-result turn so the next provider call
			// has the full context.
			_ = a.sessions.AppendToolResultTurn(ctx, msg.Identity, sessionID, tc.ID, result)
			conversation = append(conversation,
				providers.Message{Role: providers.RoleAssistant, ToolCalls: resp.ToolCalls},
				providers.Message{Role: providers.RoleTool, ToolCallID: tc.ID, Content: result},
			)
		}
		// Persist the assistant's tool-call turn (redacted form: the
		// LLM only ever sees redacted text, so its outputs are already
		// placeholder-safe).
		_ = a.sessions.AppendAssistantTurn(ctx, msg.Identity, sessionID, resp.Content, toolResults)
		// Loop again: the provider will see the tool results and
		// (hopefully) produce a final text reply.
		reply = resp.Content
	}

	// Restore PII in the final reply before returning to the user.
	// The session stores the redacted reply (above) so PII stays out
	// of long-term storage; the user sees the restored form.
	if reply != "" {
		_ = a.sessions.AppendAssistantTurn(ctx, msg.Identity, sessionID, reply, nil)
		return privacy.Restore(reply, piiMap), nil
	}
	return reply, nil
}

// buildConversation assembles the provider message slice from the
// system prompt + recent session turns + the new user message.
func (a *Agent) buildConversation(ctx context.Context, msg channels.IncomingMessage, sessionID string) []providers.Message {
	out := []providers.Message{
		{Role: providers.RoleSystem, Content: a.cfg.SystemPrompt},
	}
	if a.sessions == nil {
		out = append(out, providers.Message{Role: providers.RoleUser, Content: msg.Text})
		return out
	}
	sess, err := a.sessions.Get(ctx, msg.Identity, sessionID)
	if err != nil || sess == nil {
		out = append(out, providers.Message{Role: providers.RoleUser, Content: msg.Text})
		return out
	}
	// Cap the history to the last 20 turns to bound prompt size.
	start := 0
	if len(sess.Turns) > 20 {
		start = len(sess.Turns) - 20
	}
	for _, t := range sess.Turns[start:] {
		m := providers.Message{Role: providers.Role(t.Role), Content: t.Content}
		if t.Role == domain.TurnRoleTool && len(t.ToolCalls) > 0 {
			m.ToolCallID = t.ToolCalls[0].ID
		}
		out = append(out, m)
	}
	// The latest user turn was just appended to the session; dedup
	// by skipping the trailing user turn and re-adding it explicitly.
	// (Simpler than tracking the appended turn ID.)
	if len(out) > 0 {
		last := out[len(out)-1]
		if last.Role == providers.RoleUser {
			out = out[:len(out)-1]
		}
	}
	out = append(out, providers.Message{Role: providers.RoleUser, Content: msg.Text})
	return out
}

// callTool invokes an MCP tool by name. Returns the result text.
func (a *Agent) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if a.mcp == nil {
		return "", errors.New("agent: mcp client is nil")
	}
	out, err := a.mcp.CallTool(ctx, name, args)
	if err != nil {
		return "", fmt.Errorf("agent: call tool %s: %w", name, err)
	}
	return strings.TrimSpace(out), nil
}
