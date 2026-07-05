package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/bot/providers"
)

// MCPHTTPClient is the production MCPClient implementation. It wraps
// the mark3labs/mcp-go streamable-HTTP client.
//
// One client per bot process; the agent loop calls ListTools/CallTool
// on every inbound message, so the client must be initialized before
// the agent starts.
type MCPHTTPClient struct {
	cfg   config.AgentConfig
	cli   *mcpclient.Client
	tools []providers.ToolDescription
}

// NewMCPHTTPClient constructs a streamable-HTTP MCP client. It does
// not call Initialize; callers must call Init before using the client.
func NewMCPHTTPClient(cfg config.AgentConfig) (*MCPHTTPClient, error) {
	if cfg.MCPURL == "" {
		return nil, fmt.Errorf("mcp: url is empty")
	}
	opts := []mcptransport.StreamableHTTPCOption{}
	if cfg.MCPBasicUser != "" || cfg.MCPBasicPass != "" {
		// Add HTTP Basic auth via headers.
		headers := map[string]string{}
		if cfg.MCPBasicUser != "" {
			req, _ := http.NewRequest(http.MethodGet, cfg.MCPURL, nil)
			req.SetBasicAuth(cfg.MCPBasicUser, cfg.MCPBasicPass)
			headers["Authorization"] = req.Header.Get("Authorization")
		}
		opts = append(opts, mcptransport.WithHTTPHeaders(headers))
	}
	cli, err := mcpclient.NewStreamableHttpClient(cfg.MCPURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("mcp: new client: %w", err)
	}
	return &MCPHTTPClient{cfg: cfg, cli: cli}, nil
}

// Init sends the MCP initialize request and caches the tool list.
func (c *MCPHTTPClient) Init(ctx context.Context) error {
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{
		Name:    "vikingbot",
		Version: "0.1.0",
	}
	req.Params.Capabilities = mcp.ClientCapabilities{}
	if _, err := c.cli.Initialize(ctx, req); err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	// Cache tools.
	tools, err := c.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("mcp: list tools: %w", err)
	}
	c.tools = tools
	return nil
}

// ListTools implements MCPClient.
func (c *MCPHTTPClient) ListTools(ctx context.Context) ([]providers.ToolDescription, error) {
	res, err := c.cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools: %w", err)
	}
	out := make([]providers.ToolDescription, 0, len(res.Tools))
	for _, t := range res.Tools {
		td := providers.ToolDescription{
			Name:        t.Name,
			Description: t.Description,
		}
		// InputSchema is a struct; marshal and re-parse as a generic
		// map so the provider can pass it through.
		if raw, err := json.Marshal(t.InputSchema); err == nil {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err == nil {
				td.Parameters = m
			}
		}
		out = append(out, td)
	}
	return out, nil
}

// CallTool implements MCPClient.
func (c *MCPHTTPClient) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.cli.CallTool(ctx, req)
	if err != nil {
		return "", fmt.Errorf("mcp: call tool %s: %w", name, err)
	}
	// Concatenate text content blocks.
	var sb []byte
	for _, item := range res.Content {
		if t, ok := item.(mcp.TextContent); ok {
			sb = append(sb, t.Text...)
		}
	}
	if res.IsError {
		return string(sb), fmt.Errorf("mcp: tool %s returned error", name)
	}
	return string(sb), nil
}

// Close releases the MCP client resources.
func (c *MCPHTTPClient) Close() error {
	if c.cli == nil {
		return nil
	}
	return c.cli.Close()
}
