package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/saker-ai/ctxhub/internal/parse/accessors"
	_ "github.com/saker-ai/ctxhub/internal/parse/parsers"
	_ "github.com/saker-ai/ctxhub/internal/parse/parsers/code"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
	"github.com/saker-ai/ctxhub/internal/retrieve"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// --- original structural tests ---

func TestNew_RegistersAllTools(t *testing.T) {
	srv := New()
	tools := srv.Tools()
	assert.Len(t, tools, 13)
	assert.ElementsMatch(t, ToolNames, tools)
}

func TestNew_DefaultHandlerIsStateless(t *testing.T) {
	srv := New()
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	// Stateless mode: POST without an initialized session returns a
	// JSON-RPC error response (not a session error).
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req, err := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := hs.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "tools")
	// All 13 tool names should appear in the listing.
	for _, name := range ToolNames {
		assert.Contains(t, string(raw), name, "tool %q missing", name)
	}
}

func TestToolNames_AllPresent(t *testing.T) {
	expected := []string{
		"find", "search", "read", "list", "remember", "add_resource",
		"grep", "glob", "code_outline", "code_search", "code_expand",
		"forget", "health",
	}
	assert.ElementsMatch(t, expected, ToolNames)
	for _, name := range ToolNames {
		assert.NotEmpty(t, toolDescription(name), "description for %q", name)
	}
}

// --- graceful degradation: tools without services return errors ---

func TestToolsWithoutServices_ReturnServiceNotConfigured(t *testing.T) {
	// With no services injected, every tool (except health, which
	// reports "not configured" as status) must return a structured
	// error result mentioning "service not configured" or, for health,
	// a status map with "not configured" values.
	srv := New()
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"find", map[string]any{"query": "x"}},
		{"search", map[string]any{"query": "x"}},
		{"read", map[string]any{"uri": "/x"}},
		{"list", map[string]any{}},
		{"remember", map[string]any{"content": "x"}},
		{"add_resource", map[string]any{"path": "/x", "content": "y"}},
		{"grep", map[string]any{"pattern": "x"}},
		{"glob", map[string]any{"pattern": "*.md"}},
		{"code_outline", map[string]any{"uri": "/x.py"}},
		{"code_search", map[string]any{"query": "x"}},
		{"code_expand", map[string]any{"uri": "/x.py", "symbol": "y"}},
		{"forget", map[string]any{"uri": "/x"}},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			raw := callTool(t, hs, c.tool, c.args)
			assert.Contains(t, raw, "not configured", "tool %s should report service not configured", c.tool)
		})
	}
}

func TestHealth_NoServices_ReportsNotConfigured(t *testing.T) {
	srv := New()
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "health", nil)
	assert.Contains(t, raw, "ragfs")
	assert.Contains(t, raw, "not configured")
}

// --- basic auth ---

func TestBasicAuth_RejectsMissingCredentials(t *testing.T) {
	srv := New(WithBasicAuth(BasicAuthConfig{
		Enabled:  true,
		Username: "alice",
		Password: "secret",
	}))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	req, err := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hs.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Basic realm=")
}

func TestBasicAuth_AcceptsValidCredentials(t *testing.T) {
	srv := New(WithBasicAuth(BasicAuthConfig{
		Enabled:  true,
		Username: "alice",
		Password: "secret",
	}))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req, err := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.SetBasicAuth("alice", "secret")

	resp, err := hs.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestBasicAuth_RejectsWrongPassword(t *testing.T) {
	srv := New(WithBasicAuth(BasicAuthConfig{
		Enabled:  true,
		Username: "alice",
		Password: "secret",
	}))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	req, err := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("alice", "wrong")
	resp, err := hs.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// --- ragfs-backed tool tests ---

// newMemFS returns a memfs pre-seeded with a small doc tree used by
// the tool tests below:
//
//	/docs/hello.md   -> "hello world from docs"
//	/docs/nested/greet.md -> "greetings from nested"
//	/code/sample.py  -> "def greet(name):\n    print(name)\n"
func newMemFS(t *testing.T) ragfs.FileSystem {
	t.Helper()
	fs := memfs.New("test")
	ctx := context.Background()
	require.NoError(t, fs.Mkdir(ctx, "/docs", 0o755))
	require.NoError(t, fs.Mkdir(ctx, "/docs/nested", 0o755))
	require.NoError(t, fs.Mkdir(ctx, "/code", 0o755))
	require.NoError(t, fs.Write(ctx, "/docs/hello.md", strings.NewReader("hello world from docs"), 0o644))
	require.NoError(t, fs.Write(ctx, "/docs/nested/greet.md", strings.NewReader("greetings from nested"), 0o644))
	require.NoError(t, fs.Write(ctx, "/code/sample.py", strings.NewReader("def greet(name):\n    print(name)\n"), 0o644))
	return fs
}

func TestTool_Find_ReturnsMatches(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "find", map[string]any{"query": "hello"})
	assert.Contains(t, raw, "hello world from docs")
	assert.Contains(t, raw, "/docs/hello.md")
}

func TestTool_Grep_FiltersByPattern(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "grep", map[string]any{"pattern": "greetings"})
	assert.Contains(t, raw, "greetings from nested")
	assert.Contains(t, raw, "/docs/nested/greet.md")
}

func TestTool_Read_ReturnsContent(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "read", map[string]any{"uri": "/docs/hello.md"})
	assert.Contains(t, raw, "hello world from docs")
}

func TestTool_Read_AbstractLayer(t *testing.T) {
	fs := newMemFS(t)
	ctx := context.Background()
	require.NoError(t, ragfs.WriteAbstract(ctx, fs, "/docs/hello.md", "short abstract"))

	srv := New(WithRAGFS(fs))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "read", map[string]any{"uri": "/docs/hello.md", "layer": "abstract"})
	assert.Contains(t, raw, "short abstract")
}

func TestTool_List_ReturnsEntries(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "list", map[string]any{"path": "/docs"})
	assert.Contains(t, raw, "hello.md")
	assert.Contains(t, raw, "nested")
}

func TestTool_AddResource_ThenRead(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	_ = callTool(t, hs, "add_resource", map[string]any{
		"path":    "/notes/added.txt",
		"content": "freshly written",
	})
	raw := callTool(t, hs, "read", map[string]any{"uri": "/notes/added.txt"})
	assert.Contains(t, raw, "freshly written")
}

func TestTool_Remember_WritesToAccountPath(t *testing.T) {
	srv := New(WithRAGFS(memfs.New("test")))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "remember", map[string]any{
		"content": "remembered memory",
		"type":    "note",
		"account": "alice",
	})
	assert.Contains(t, raw, "/accounts/alice/memories/")
	// Verify by reading back via the read tool.
	raw = callTool(t, hs, "list", map[string]any{"path": "/accounts/alice/memories"})
	assert.Contains(t, raw, ".md")
}

func TestTool_Glob_MatchesByBaseName(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "glob", map[string]any{"pattern": "*.md"})
	assert.Contains(t, raw, "/docs/hello.md")
	assert.Contains(t, raw, "/docs/nested/greet.md")
}

func TestTool_Glob_MatchesByPath(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "glob", map[string]any{"pattern": "docs/*"})
	// docs/hello.md and docs/nested should match the docs/* pattern.
	assert.Contains(t, raw, "/docs/hello.md")
}

func TestTool_Forget_DeletesResource(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	_ = callTool(t, hs, "forget", map[string]any{"uri": "/docs/hello.md"})
	raw := callTool(t, hs, "read", map[string]any{"uri": "/docs/hello.md"})
	// read after delete must surface a not-found error result.
	assert.Contains(t, raw, "isError\":true")
	assert.Contains(t, raw, "not found")
}

func TestTool_CodeOutline_ReturnsSymbols(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "code_outline", map[string]any{"uri": "/code/sample.py"})
	// tree-sitter python emits a "function_definition" node type for
	// the def greet() block. The CodeParser sets Title to child.Type()
	// (e.g. "function_definition").
	assert.Contains(t, raw, "function_definition")
}

func TestTool_CodeExpand_ReturnsSymbolText(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "code_expand", map[string]any{
		"uri":    "/code/sample.py",
		"symbol": "function_definition",
	})
	assert.Contains(t, raw, "def greet(name):")
	assert.Contains(t, raw, "print(name)")
}

func TestTool_CodeExpand_NotFound(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "code_expand", map[string]any{
		"uri":    "/code/sample.py",
		"symbol": "nonexistent_symbol",
	})
	assert.Contains(t, raw, "not found")
}

func TestTool_CodeSearch_FiltersByLanguage(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	// Search for "greet" but only in python files. The .md files
	// containing "greetings" should be filtered out.
	raw := callTool(t, hs, "code_search", map[string]any{
		"query":    "greet",
		"language": "python",
	})
	assert.Contains(t, raw, "/code/sample.py")
	assert.NotContains(t, raw, "/docs/nested/greet.md")
}

func TestTool_CodeSearch_UnsupportedLanguage(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "code_search", map[string]any{
		"query":    "x",
		"language": "klingon",
	})
	assert.Contains(t, raw, "unsupported language")
}

// --- vectordb / retrieve-backed tools ---

type stubRetriever struct {
	docs []retrieve.Document
}

func (r *stubRetriever) Retrieve(ctx context.Context, req retrieve.RetrieveRequest) (*retrieve.RetrieveResponse, error) {
	return &retrieve.RetrieveResponse{Query: req.Query, Documents: r.docs}, nil
}

func TestTool_Search_ReturnsDocuments(t *testing.T) {
	docs := []retrieve.Document{
		{URI: "viking://agent/resources/x", Content: "x", Score: 0.9, Level: retrieve.Level0Abstract},
	}
	srv := New(WithRetrieve(&stubRetriever{docs: docs}))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "search", map[string]any{"query": "x", "top_k": 5})
	assert.Contains(t, raw, "viking://agent/resources/x")
	assert.Contains(t, raw, "0.9")
}

// stubVectordbAdapter is a minimal CollectionAdapter for the health
// tool. Only Close is exercised in practice; the rest return nil/zero.
type stubVectordbAdapter struct{}

func (stubVectordbAdapter) EnsureCollection(context.Context, vectordb.CollectionSchema) error { return nil }
func (stubVectordbAdapter) DropCollection(context.Context, string) error                     { return nil }
func (stubVectordbAdapter) ListCollections(context.Context) ([]string, error)                { return nil, nil }
func (stubVectordbAdapter) Upsert(context.Context, string, []vectordb.Vector) error          { return nil }
func (stubVectordbAdapter) Delete(context.Context, string, []string) error                   { return nil }
func (stubVectordbAdapter) Search(context.Context, vectordb.SearchParams) (*vectordb.SearchResult, error) {
	return &vectordb.SearchResult{}, nil
}
func (stubVectordbAdapter) Get(context.Context, string, string) (*vectordb.Vector, error) {
	return nil, nil
}
func (stubVectordbAdapter) Count(context.Context, string) (int64, error) { return 0, nil }
func (stubVectordbAdapter) Close() error                                  { return nil }

func TestTool_Health_WithServices_ReportsOk(t *testing.T) {
	srv := New(WithRAGFS(newMemFS(t)), WithVectorDB(stubVectordbAdapter{}))
	hs := httptest.NewServer(srv.HTTPHandler())
	defer hs.Close()

	raw := callTool(t, hs, "health", nil)
	assert.Contains(t, raw, "ok")
	assert.NotContains(t, raw, "not configured")
}

// --- helpers ---

// callTool POSTs a JSON-RPC tools/call request and returns the raw
// response body as a string. It fails the test on any transport error
// or non-200 status.
func callTool(t *testing.T, hs *httptest.Server, tool string, args map[string]any) string {
	t.Helper()
	argBytes, _ := json.Marshal(args)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + string(argBytes) + `}}`
	req, err := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := hs.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "tool %s HTTP status", tool)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(raw)
}
