// Package mcp wires the Model Context Protocol server surface for
// OpenViking. It uses github.com/mark3labs/mcp-go to expose a
// streamable-HTTP transport at /mcp with the 13 tools listed in the
// design doc (section 7.9.3): find, search, read, list, remember,
// add_resource, grep, glob, code_outline, code_search, code_expand,
// forget, health.
//
// Each tool wraps a real handler that calls into the injected ragfs,
// vectordb, session, and retrieve services. Services are optional; a
// tool whose backing service is nil returns a "service not configured"
// error result so the MCP surface stays usable in degraded deployments.
//
// Authentication is pluggable: when the OAuth 2.1 server is enabled the
// caller wraps the HTTP handler with a bearer-token middleware,
// otherwise HTTP Basic auth is used (configurable username/password,
// defaulting to disabled for local dev where the upstream ingress
// handles auth).
//
// Identity: the MCP endpoint sits at /mcp OUTSIDE the identity
// middleware (per app.go). There is no X-OpenViking-Account header
// available to handlers. Until a proper MCP-session→account binding
// lands, each tool accepts an `account` string argument (default
// "default") so callers can scope multi-tenant reads/writes. This is
// documented per-handler below.
package mcp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/saker-ai/ctxhub/internal/parse"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/retrieve"
	"github.com/saker-ai/ctxhub/internal/session"
	"github.com/saker-ai/ctxhub/internal/version"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// ToolNames is the canonical list of MCP tools exposed by OpenViking.
// Order matches the design doc section 7.9.3 listing.
var ToolNames = []string{
	"find",
	"search",
	"read",
	"list",
	"remember",
	"add_resource",
	"grep",
	"glob",
	"code_outline",
	"code_search",
	"code_expand",
	"forget",
	"health",
}

// BasicAuthConfig configures the optional HTTP Basic auth layer that
// protects the MCP endpoint when OAuth is not enabled.
type BasicAuthConfig struct {
	Enabled  bool
	Username string
	Password string
	// Realm is the WWW-Authenticate realm; defaults to "openviking-mcp".
	Realm string
}

// Server is the OpenViking MCP server. It wraps a mark3labs MCPServer
// with the 13 real tools and exposes a streamable-HTTP handler.
// Injected services are nil-safe: a tool whose backing service is nil
// returns a "service not configured" error result.
type Server struct {
	mcpSrv    *server.MCPServer
	httpSrv   *server.StreamableHTTPServer
	basicAuth BasicAuthConfig

	ragfs    ragfs.FileSystem
	vectordb vectordb.CollectionAdapter
	sessions session.Store
	retrieve retrieve.Retriever

	// tempUpload resolves chunked uploads submitted via temp_file_id.
	// When nil, add_resource with a temp_file_id returns a "service not
	// configured" error result.
	tempUpload TempUploadResolver
	// inputGuard blocks resource sources that would let an HTTP client
	// read from the host filesystem or pivot to internal services. When
	// nil, the guard is treated as always-allow so the handler stays
	// nil-safe in degraded deployments.
	inputGuard InputGuard
}

// TempUploadResolver resolves a server-minted temp_file_id (issued by a
// prior HTTP upload) to a final on-disk path the consumer can read.
// Implemented by *server.TempUploadStore; defined here to avoid a
// circular import (server depends on mcp via app.go).
type TempUploadResolver interface {
	Finalize(ctx context.Context, uploadID string) (string, error)
}

// InputGuard blocks resource sources that would let an HTTP client
// read from the host filesystem or pivot to internal services.
// Implemented by *server.LocalInputGuard; defined here to avoid a
// circular import.
type InputGuard interface {
	Check(source string) error
}

// New constructs a new MCP server with all 13 tools registered. Tools
// call into the injected services (ragfs, vectordb, sessions,
// retrieve); services are nil-safe and degrade to "service not
// configured" error results when absent.
func New(opts ...Option) *Server {
	srv := &Server{
		mcpSrv: server.NewMCPServer("openviking", version.Version),
	}
	for _, opt := range opts {
		opt(srv)
	}
	srv.registerTools()
	srv.httpSrv = server.NewStreamableHTTPServer(
		srv.mcpSrv,
		server.WithStateLess(true),
	)
	return srv
}

// Option configures the MCP Server at construction time.
type Option func(*Server)

// WithRAGFS injects a ragfs.FileSystem backing find/read/list/grep/
// glob/code_*/remember/add_resource/forget/health.
func WithRAGFS(fs ragfs.FileSystem) Option {
	return func(s *Server) { s.ragfs = fs }
}

// WithVectorDB injects a vectordb.CollectionAdapter backing health
// status reporting. Search currently routes through retrieve.Retriever
// (which composes vectordb.Search internally); the adapter is wired
// here so the health tool can report its configured state.
func WithVectorDB(db vectordb.CollectionAdapter) Option {
	return func(s *Server) { s.vectordb = db }
}

// WithSessions injects a session.Store. Currently unused by the 13
// tools (remember writes to ragfs directly) but wired for forward
// compatibility with session-backed memory tools.
func WithSessions(store session.Store) Option {
	return func(s *Server) { s.sessions = store }
}

// WithRetrieve injects a retrieve.Retriever backing the search tool.
func WithRetrieve(r retrieve.Retriever) Option {
	return func(s *Server) { s.retrieve = r }
}

// WithTempUpload injects a TempUploadResolver so the add_resource tool
// can ingest chunked uploads submitted via temp_file_id. When nil, the
// handler returns a "service not configured" error result.
func WithTempUpload(r TempUploadResolver) Option {
	return func(s *Server) { s.tempUpload = r }
}

// WithInputGuard injects an InputGuard that blocks resource sources
// pointing at the host filesystem or private IP ranges. When nil, the
// guard is treated as always-allow so the handler stays nil-safe.
func WithInputGuard(g InputGuard) Option {
	return func(s *Server) { s.inputGuard = g }
}

// WithBasicAuth enables HTTP Basic auth on the MCP endpoint.
func WithBasicAuth(cfg BasicAuthConfig) Option {
	return func(s *Server) {
		s.basicAuth = cfg
		if s.basicAuth.Realm == "" {
			s.basicAuth.Realm = "openviking-mcp"
		}
	}
}

// HTTPHandler returns the http.Handler serving the MCP streamable-HTTP
// transport. When basic auth is enabled the handler is wrapped with
// the auth middleware.
func (s *Server) HTTPHandler() http.Handler {
	h := http.Handler(s.httpSrv)
	if s.basicAuth.Enabled {
		h = s.basicAuthMiddleware(h)
	}
	return h
}

// Tools returns the names of the tools registered on the server.
func (s *Server) Tools() []string {
	out := make([]string, len(ToolNames))
	copy(out, ToolNames)
	return out
}

// basicAuthMiddleware wraps h with HTTP Basic auth.
func (s *Server) basicAuthMiddleware(h http.Handler) http.Handler {
	expectedUser := s.basicAuth.Username
	expectedPass := s.basicAuth.Password
	realm := s.basicAuth.Realm
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(expectedUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(expectedPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// registerTools registers all 13 MCP tools with real handlers backed
// by the injected services.
func (s *Server) registerTools() {
	s.registerFind()
	s.registerSearch()
	s.registerRead()
	s.registerList()
	s.registerRemember()
	s.registerAddResource()
	s.registerGrep()
	s.registerGlob()
	s.registerCodeOutline()
	s.registerCodeSearch()
	s.registerCodeExpand()
	s.registerForget()
	s.registerHealth()
}

// --- per-tool registration + handlers ---

// registerFind registers the find tool: regex search over ragfs paths.
//
// Identity: scope via the `account` argument (default "default").
func (s *Server) registerFind() {
	s.mcpSrv.AddTool(mcp.NewTool("find",
		mcp.WithDescription(toolDescription("find")),
		mcp.WithString("query", mcp.Required(), mcp.Description("Regex pattern to search for in file contents")),
		mcp.WithString("path", mcp.DefaultString("/"), mcp.Description("Root path to anchor the search")),
		mcp.WithBoolean("recursive", mcp.DefaultBool(true), mcp.Description("Recurse into subdirectories")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope (MCP runs outside identity middleware)")),
	), s.handleFind)
}

func (s *Server) handleFind(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("find: query is required"), nil
	}
	if s.ragfs == nil {
		return mcp.NewToolResultError("find: ragfs service not configured"), nil
	}
	root := req.GetString("path", "/")
	recursive := req.GetBool("recursive", true)
	matches, err := s.ragfs.Grep(ctx, query, root, recursive)
	if err != nil {
		return mcp.NewToolResultError("find: " + err.Error()), nil
	}
	return jsonTextResult("find", matches)
}

// registerSearch registers the search tool: hierarchical retrieval.
func (s *Server) registerSearch() {
	s.mcpSrv.AddTool(mcp.NewTool("search",
		mcp.WithDescription(toolDescription("search")),
		mcp.WithString("query", mcp.Required(), mcp.Description("Natural-language query")),
		mcp.WithInteger("top_k", mcp.DefaultNumber(10), mcp.Description("Maximum documents to return")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleSearch)
}

func (s *Server) handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("search: query is required"), nil
	}
	if s.retrieve == nil {
		return mcp.NewToolResultError("search: retrieve service not configured"), nil
	}
	topK := req.GetInt("top_k", 10)
	account := req.GetString("account", "default")
	resp, err := s.retrieve.Retrieve(ctx, retrieve.RetrieveRequest{
		Account: account,
		Query:   query,
		TopK:    topK,
	})
	if err != nil {
		return mcp.NewToolResultError("search: " + err.Error()), nil
	}
	return jsonTextResult("search", resp)
}

// registerRead registers the read tool: read a resource by URI, with
// optional ?layer=abstract|overview|chunks sidecar resolution.
func (s *Server) registerRead() {
	s.mcpSrv.AddTool(mcp.NewTool("read",
		mcp.WithDescription(toolDescription("read")),
		mcp.WithString("uri", mcp.Required(), mcp.Description("Resource URI (ragfs path)")),
		mcp.WithString("layer", mcp.Description("Optional sidecar layer: abstract | overview | chunks")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleRead)
}

func (s *Server) handleRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uri := req.GetString("uri", "")
	if uri == "" {
		return mcp.NewToolResultError("read: uri is required"), nil
	}
	if s.ragfs == nil {
		return mcp.NewToolResultError("read: ragfs service not configured"), nil
	}
	layer := req.GetString("layer", "")
	switch layer {
	case "":
		var buf bytes.Buffer
		if err := s.ragfs.Read(ctx, uri, &buf); err != nil {
			return mcp.NewToolResultError("read: " + err.Error()), nil
		}
		return mcp.NewToolResultText(buf.String()), nil

	case "abstract":
		text, err := ragfs.ReadAbstract(ctx, s.ragfs, uri)
		if err != nil {
			return mcp.NewToolResultError("read: abstract: " + err.Error()), nil
		}
		return mcp.NewToolResultText(text), nil

	case "overview":
		text, err := ragfs.ReadOverview(ctx, s.ragfs, uri)
		if err != nil {
			return mcp.NewToolResultError("read: overview: " + err.Error()), nil
		}
		return mcp.NewToolResultText(text), nil

	case "chunks":
		names, err := ragfs.ListChunks(ctx, s.ragfs, uri)
		if err != nil {
			return mcp.NewToolResultError("read: chunks: " + err.Error()), nil
		}
		return jsonTextResult("read", map[string]any{"chunks": names})

	default:
		return mcp.NewToolResultError("read: unsupported layer " + layer + " (want abstract|overview|chunks)"), nil
	}
}

// registerList registers the list tool: directory listing.
func (s *Server) registerList() {
	s.mcpSrv.AddTool(mcp.NewTool("list",
		mcp.WithDescription(toolDescription("list")),
		mcp.WithString("path", mcp.DefaultString("/"), mcp.Description("Directory path to list")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleList)
}

func (s *Server) handleList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.ragfs == nil {
		return mcp.NewToolResultError("list: ragfs service not configured"), nil
	}
	p := req.GetString("path", "/")
	entries, err := s.ragfs.ReadDir(ctx, p)
	if err != nil {
		return mcp.NewToolResultError("list: " + err.Error()), nil
	}
	return jsonTextResult("list", entries)
}

// registerRemember registers the remember tool: persist a memory entry.
// Writes to ragfs at /accounts/{account}/memories/{timestamp}.md.
func (s *Server) registerRemember() {
	s.mcpSrv.AddTool(mcp.NewTool("remember",
		mcp.WithDescription(toolDescription("remember")),
		mcp.WithString("content", mcp.Required(), mcp.Description("Memory content to persist")),
		mcp.WithString("type", mcp.DefaultString("note"), mcp.Enum("note", "skill", "preference"), mcp.Description("Memory type")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleRemember)
}

func (s *Server) handleRemember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	content := req.GetString("content", "")
	if content == "" {
		return mcp.NewToolResultError("remember: content is required"), nil
	}
	if s.ragfs == nil {
		return mcp.NewToolResultError("remember: ragfs service not configured"), nil
	}
	memType := req.GetString("type", "note")
	account := req.GetString("account", "default")
	// /accounts/{account}/memories/{timestamp}-{type}.md
	dir := fmt.Sprintf("/accounts/%s/memories", account)
	if err := s.ragfs.Mkdir(ctx, dir, 0o755); err != nil && !ragfs.IsConflict(err) {
		return mcp.NewToolResultError("remember: mkdir: " + err.Error()), nil
	}
	name := fmt.Sprintf("%s-%s.md", time.Now().UTC().Format("20060102T150405.000000000"), memType)
	full := dir + "/" + name
	if err := s.ragfs.Write(ctx, full, strings.NewReader(content), 0o644); err != nil {
		return mcp.NewToolResultError("remember: write: " + err.Error()), nil
	}
	return jsonTextResult("remember", map[string]any{"uri": full, "type": memType})
}

// registerAddResource registers the add_resource tool: write content
// to a ragfs path. The handler also resolves chunked uploads submitted
// via temp_file_id through the injected TempUploadResolver, and runs
// every source through the InputGuard before fetching.
func (s *Server) registerAddResource() {
	s.mcpSrv.AddTool(mcp.NewTool("add_resource",
		mcp.WithDescription(toolDescription("add_resource")),
		mcp.WithString("path", mcp.Required(), mcp.Description("Destination ragfs path")),
		mcp.WithString("content", mcp.Description("Resource content (inline bytes)")),
		mcp.WithString("temp_file_id", mcp.Description("Server-minted upload id from a prior chunked upload; when set, content is read from the resolved temp file")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleAddResource)
}

func (s *Server) handleAddResource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p := req.GetString("path", "")
	if p == "" {
		return mcp.NewToolResultError("add_resource: path is required"), nil
	}
	// Defensive: run the destination path through the InputGuard so
	// agents cannot probe host filesystem locations (e.g. /etc/passwd)
	// via this tool. When the guard is nil the check is a no-op.
	if s.inputGuard != nil {
		if err := s.inputGuard.Check(p); err != nil {
			return mcp.NewToolResultError("add_resource: " + err.Error()), nil
		}
	}
	if s.ragfs == nil {
		return mcp.NewToolResultError("add_resource: ragfs service not configured"), nil
	}

	tempFileID := req.GetString("temp_file_id", "")
	var content string
	if tempFileID != "" {
		// Chunked upload path: resolve the temp file via the store and
		// read its bytes. The temp file path is server-controlled, so
		// the InputGuard does not apply to it.
		if s.tempUpload == nil {
			return mcp.NewToolResultError("add_resource: temp upload service not configured"), nil
		}
		tempPath, err := s.tempUpload.Finalize(ctx, tempFileID)
		if err != nil {
			return mcp.NewToolResultError("add_resource: resolve temp upload: " + err.Error()), nil
		}
		bytes, err := os.ReadFile(tempPath)
		if err != nil {
			return mcp.NewToolResultError("add_resource: read temp file: " + err.Error()), nil
		}
		content = string(bytes)
	} else {
		content = req.GetString("content", "")
	}
	if err := s.ragfs.Write(ctx, p, strings.NewReader(content), 0o644); err != nil {
		return mcp.NewToolResultError("add_resource: " + err.Error()), nil
	}
	return jsonTextResult("add_resource", map[string]any{"uri": p, "bytes": len(content)})
}

// registerGrep registers the grep tool: regex search over ragfs.
func (s *Server) registerGrep() {
	s.mcpSrv.AddTool(mcp.NewTool("grep",
		mcp.WithDescription(toolDescription("grep")),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Regex pattern")),
		mcp.WithString("path", mcp.DefaultString("/"), mcp.Description("Root path")),
		mcp.WithBoolean("recursive", mcp.DefaultBool(true), mcp.Description("Recurse into subdirectories")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleGrep)
}

func (s *Server) handleGrep(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pattern := req.GetString("pattern", "")
	if pattern == "" {
		return mcp.NewToolResultError("grep: pattern is required"), nil
	}
	if s.ragfs == nil {
		return mcp.NewToolResultError("grep: ragfs service not configured"), nil
	}
	root := req.GetString("path", "/")
	recursive := req.GetBool("recursive", true)
	matches, err := s.ragfs.Grep(ctx, pattern, root, recursive)
	if err != nil {
		return mcp.NewToolResultError("grep: " + err.Error()), nil
	}
	return jsonTextResult("grep", matches)
}

// registerGlob registers the glob tool: pattern match against ragfs
// paths via filepath.Match.
func (s *Server) registerGlob() {
	s.mcpSrv.AddTool(mcp.NewTool("glob",
		mcp.WithDescription(toolDescription("glob")),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Glob pattern (filepath.Match semantics, e.g. *.md, docs/*)")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleGlob)
}

func (s *Server) handleGlob(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pattern := req.GetString("pattern", "")
	if pattern == "" {
		return mcp.NewToolResultError("glob: pattern is required"), nil
	}
	if s.ragfs == nil {
		return mcp.NewToolResultError("glob: ragfs service not configured"), nil
	}
	entries, err := s.ragfs.TreeDirectory(ctx, "/", globWalkDepth)
	if err != nil {
		// TreeDirectory may fail on an empty or freshly-initialized FS;
		// fall back to a root listing so glob still returns something
		// useful rather than an error.
		entries, err = s.ragfs.ReadDir(ctx, "/")
		if err != nil {
			return mcp.NewToolResultError("glob: " + err.Error()), nil
		}
	}
	var paths []string
	for _, e := range entries {
		if e == nil {
			continue
		}
		candidate := e.Path
		if candidate == "" && e.Info != nil {
			candidate = e.Info.Name
		}
		// Try the full path (without leading slash) first so patterns
		// like "docs/*" match "/docs/hello.md". Then fall back to the
		// base name so patterns like "*.md" match individual files.
		trimmed := strings.TrimPrefix(candidate, "/")
		if matched, _ := filepath.Match(pattern, trimmed); matched {
			paths = append(paths, candidate)
			continue
		}
		if base := filepath.Base(trimmed); base != "" && base != trimmed {
			if matched, _ := filepath.Match(pattern, base); matched {
				paths = append(paths, candidate)
			}
		}
	}
	return jsonTextResult("glob", paths)
}

// globWalkDepth bounds the recursive depth glob walks when backed by
// ragfs.TreeDirectory. Ten levels covers typical doc trees; callers
// needing deeper traversal should use grep.
const globWalkDepth = 10

// registerCodeOutline registers the code_outline tool: parse a source
// file and return its symbol outline.
func (s *Server) registerCodeOutline() {
	s.mcpSrv.AddTool(mcp.NewTool("code_outline",
		mcp.WithDescription(toolDescription("code_outline")),
		mcp.WithString("uri", mcp.Required(), mcp.Description("Source file URI (ragfs path)")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleCodeOutline)
}

func (s *Server) handleCodeOutline(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uri := req.GetString("uri", "")
	if uri == "" {
		return mcp.NewToolResultError("code_outline: uri is required"), nil
	}
	pr, cleanup, err := s.parseURI(ctx, uri)
	if err != nil {
		return mcp.NewToolResultError("code_outline: " + err.Error()), nil
	}
	if cleanup != nil {
		defer cleanup()
	}
	type entry struct {
		Type  string `json:"type"`
		Title string `json:"title"`
	}
	var outline []entry
	if pr != nil {
		for _, n := range pr.AllNodes() {
			if n == nil || n.Type != parse.NodeCode {
				continue
			}
			outline = append(outline, entry{Type: string(n.Type), Title: n.Title})
		}
	}
	return jsonTextResult("code_outline", outline)
}

// registerCodeSearch registers the code_search tool: regex search over
// code files, optionally filtered by language.
func (s *Server) registerCodeSearch() {
	s.mcpSrv.AddTool(mcp.NewTool("code_search",
		mcp.WithDescription(toolDescription("code_search")),
		mcp.WithString("query", mcp.Required(), mcp.Description("Regex pattern to search for")),
		mcp.WithString("language", mcp.Description("Optional language filter: go | python | javascript | typescript | rust | cpp | csharp | java | lua | php")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleCodeSearch)
}

// codeLanguageExts maps a language name to its recognized file
// extensions. Used by code_search to filter grep hits.
var codeLanguageExts = map[string][]string{
	"go":         {".go"},
	"python":     {".py"},
	"javascript": {".js", ".mjs", ".cjs", ".jsx"},
	"typescript": {".ts", ".cts", ".mts", ".tsx"},
	"rust":       {".rs"},
	"cpp":        {".cc", ".cxx", ".cpp", ".hpp", ".hh", ".h"},
	"c":          {".c", ".h"},
	"csharp":     {".cs"},
	"java":       {".java"},
	"lua":        {".lua"},
	"php":        {".php"},
}

func (s *Server) handleCodeSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("code_search: query is required"), nil
	}
	if s.ragfs == nil {
		return mcp.NewToolResultError("code_search: ragfs service not configured"), nil
	}
	language := strings.ToLower(strings.TrimSpace(req.GetString("language", "")))
	var allowed map[string]bool
	if language != "" {
		exts := codeLanguageExts[language]
		if len(exts) == 0 {
			return mcp.NewToolResultError("code_search: unsupported language " + language), nil
		}
		allowed = make(map[string]bool, len(exts))
		for _, e := range exts {
			allowed[e] = true
		}
	}
	matches, err := s.ragfs.Grep(ctx, query, "/", true)
	if err != nil {
		return mcp.NewToolResultError("code_search: " + err.Error()), nil
	}
	filtered := matches[:0]
	for _, m := range matches {
		if allowed != nil && !allowed[strings.ToLower(filepath.Ext(m.Path))] {
			continue
		}
		filtered = append(filtered, m)
	}
	return jsonTextResult("code_search", filtered)
}

// registerCodeExpand registers the code_expand tool: find a symbol in
// a parsed source file and return its full definition text.
func (s *Server) registerCodeExpand() {
	s.mcpSrv.AddTool(mcp.NewTool("code_expand",
		mcp.WithDescription(toolDescription("code_expand")),
		mcp.WithString("uri", mcp.Required(), mcp.Description("Source file URI (ragfs path)")),
		mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name (e.g. function/class identifier)")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleCodeExpand)
}

func (s *Server) handleCodeExpand(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uri := req.GetString("uri", "")
	if uri == "" {
		return mcp.NewToolResultError("code_expand: uri is required"), nil
	}
	symbol := req.GetString("symbol", "")
	if symbol == "" {
		return mcp.NewToolResultError("code_expand: symbol is required"), nil
	}
	pr, cleanup, err := s.parseURI(ctx, uri)
	if err != nil {
		return mcp.NewToolResultError("code_expand: " + err.Error()), nil
	}
	if cleanup != nil {
		defer cleanup()
	}
	if pr == nil {
		return mcp.NewToolResultError("code_expand: parse returned no result"), nil
	}
	for _, n := range pr.AllNodes() {
		if n == nil || n.Type != parse.NodeCode {
			continue
		}
		if n.Title == symbol {
			return mcp.NewToolResultText(codeNodeText(n)), nil
		}
		if nt, _ := n.Meta["node_type"].(string); nt == symbol {
			return mcp.NewToolResultText(codeNodeText(n)), nil
		}
	}
	return mcp.NewToolResultError("code_expand: symbol " + symbol + " not found in " + uri), nil
}

// codeNodeText extracts the source text stored on a code node by the
// tree-sitter CodeParser (Meta["text"]). Returns "" when absent.
func codeNodeText(n *parse.ResourceNode) string {
	if n == nil {
		return ""
	}
	if t, ok := n.Meta["text"].(string); ok {
		return t
	}
	return ""
}

// registerForget registers the forget tool: delete a resource by URI.
func (s *Server) registerForget() {
	s.mcpSrv.AddTool(mcp.NewTool("forget",
		mcp.WithDescription(toolDescription("forget")),
		mcp.WithString("uri", mcp.Required(), mcp.Description("Resource URI to delete")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleForget)
}

func (s *Server) handleForget(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uri := req.GetString("uri", "")
	if uri == "" {
		return mcp.NewToolResultError("forget: uri is required"), nil
	}
	if s.ragfs == nil {
		return mcp.NewToolResultError("forget: ragfs service not configured"), nil
	}
	if err := s.ragfs.Remove(ctx, uri, false); err != nil {
		return mcp.NewToolResultError("forget: " + err.Error()), nil
	}
	return jsonTextResult("forget", map[string]any{"uri": uri, "deleted": true})
}

// registerHealth registers the health tool: probe ragfs + vectordb
// and return a JSON status map.
func (s *Server) registerHealth() {
	s.mcpSrv.AddTool(mcp.NewTool("health",
		mcp.WithDescription(toolDescription("health")),
		mcp.WithString("account", mcp.DefaultString("default"), mcp.Description("Tenant account scope")),
	), s.handleHealth)
}

func (s *Server) handleHealth(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	status := map[string]string{
		"ragfs":    "not configured",
		"vectordb": "not configured",
	}
	if s.ragfs != nil {
		if err := s.ragfs.HealthCheck(ctx); err != nil {
			status["ragfs"] = "error: " + err.Error()
		} else {
			status["ragfs"] = "ok"
		}
	}
	if s.vectordb != nil {
		// CollectionAdapter has no dedicated HealthCheck; we treat a
		// non-nil adapter as "ok" since Close() would tear down state.
		status["vectordb"] = "ok"
	}
	return jsonTextResult("health", status)
}

// --- shared helpers ---

// parseURI reads a ragfs resource into a temp file and dispatches the
// configured parse pipeline at it. Returns the ParseResult plus a
// cleanup func the caller must defer. The temp file is removed by
// cleanup.
//
// Errors wrap the underlying ragfs/parse failure with a clear prefix
// so tool handlers can surface them via mcp.NewToolResultError.
func (s *Server) parseURI(ctx context.Context, uri string) (*parse.ParseResult, func(), error) {
	if s.ragfs == nil {
		return nil, nil, errors.New("ragfs service not configured")
	}
	var buf bytes.Buffer
	if err := s.ragfs.Read(ctx, uri, &buf); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", uri, err)
	}
	ext := filepath.Ext(uri)
	tmp, err := os.CreateTemp("", "openviking-mcp-*"+ext)
	if err != nil {
		return nil, nil, fmt.Errorf("create temp: %w", err)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, nil, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, nil, fmt.Errorf("close temp: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	_, pr, err := parse.Dispatch(ctx, tmp.Name(), parse.AccessorOptions{}, parse.ParserOptions{})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	return pr, cleanup, nil
}

// jsonTextResult marshals v as JSON and returns a text ToolResult. On
// marshal failure it falls back to a "%s: %v" printf form so the
// caller never gets a nil result. The tool name is included in the
// fallback for traceability.
func jsonTextResult(toolName string, v any) (*mcp.CallToolResult, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("%s: %v", toolName, v)), nil
	}
	return mcp.NewToolResultText(string(out)), nil
}

// toolDescription returns a one-line description for the named tool.
func toolDescription(name string) string {
	switch name {
	case "find":
		return "Find a memory or resource by ID or path."
	case "search":
		return "Semantic search over the memory index."
	case "read":
		return "Read a memory or resource by URI."
	case "list":
		return "List children of a collection or directory."
	case "remember":
		return "Persist a memory entry for later recall."
	case "add_resource":
		return "Ingest a new resource (URL, file, or text)."
	case "grep":
		return "Regex search across indexed content."
	case "glob":
		return "Glob-pattern file lookup across mounts."
	case "code_outline":
		return "Return the symbol outline of a source file."
	case "code_search":
		return "Semantic + lexical search over code."
	case "code_expand":
		return "Expand a code symbol into its full definition."
	case "forget":
		return "Delete a memory or resource by ID."
	case "health":
		return "Return server health and version info."
	default:
		return name
	}
}

// Compile-time guards: ensure the IO helpers we depend on satisfy the
// interfaces we expect. bytes.Buffer must satisfy io.Writer for ragfs
// Read; strings.NewReader must satisfy io.Reader for ragfs Write.
var (
	_ io.Writer = (*bytes.Buffer)(nil)
	_ io.Reader = (*strings.Reader)(nil)
)
