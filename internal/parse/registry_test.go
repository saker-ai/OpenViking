package parse_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	// Blank imports trigger init() registration of parsers and accessors.
	_ "github.com/saker-ai/ctxhub/internal/parse/accessors"
	_ "github.com/saker-ai/ctxhub/internal/parse/parsers"

	"github.com/saker-ai/ctxhub/internal/parse"
)

func TestRegisterAndLookupParser(t *testing.T) {
	parse.RegisterParser("test-parser", func() (parse.Parser, error) {
		return &mockParser{}, nil
	})
	f, ok := parse.LookupParser("test-parser")
	if !ok {
		t.Fatal("LookupParser returned ok=false for registered parser")
	}
	if f == nil {
		t.Fatal("LookupParser returned nil factory")
	}
}

func TestRegisterExtension(t *testing.T) {
	parse.RegisterParser("test-ext-parser", func() (parse.Parser, error) {
		return &mockParser{}, nil
	})
	parse.RegisterExtension(".testext", "test-ext-parser")
	f, ok := parse.LookupParserForExt(".testext")
	if !ok {
		t.Fatal("LookupParserForExt returned ok=false for registered extension")
	}
	if f == nil {
		t.Fatal("LookupParserForExt returned nil factory")
	}
}

func TestLookupParserForExt_Unknown(t *testing.T) {
	_, ok := parse.LookupParserForExt(".nonexistent")
	if ok {
		t.Error("LookupParserForExt returned ok=true for unknown extension")
	}
}

func TestLookupParser_Unknown(t *testing.T) {
	_, ok := parse.LookupParser("nonexistent-parser")
	if ok {
		t.Error("LookupParser returned ok=true for unknown parser")
	}
}

func TestRegisteredParsers_ContainsKnown(t *testing.T) {
	// "markdown" is registered by parsers/markdown.go init().
	names := parse.RegisteredParsers()
	found := false
	for _, n := range names {
		if n == "markdown" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("RegisteredParsers() does not contain 'markdown'; got %v", names)
	}
}

func TestRegisteredAccessors_ContainsLocal(t *testing.T) {
	// "local" is registered by accessors/local.go init().
	schemes := parse.RegisteredAccessors()
	found := false
	for _, s := range schemes {
		if s == "local" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("RegisteredAccessors() does not contain 'local'; got %v", schemes)
	}
}

func TestSchemeOf(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{"/path/to/file.md", "local"},
		{"relative/path.txt", "local"},
		{"file:///tmp/test.md", "file"},
		{"https://example.com/doc.html", "https"},
		{"http://example.com/doc.html", "http"},
		{"git+https://github.com/repo.git", "git+https"},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			got := parse.SchemeOf(tc.source)
			if got != tc.want {
				t.Errorf("SchemeOf(%q) = %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

func TestDispatch_LocalMarkdown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")
	if err := os.WriteFile(path, []byte("# Hello\n\nWorld\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	res, pr, err := parse.Dispatch(context.Background(), path, parse.AccessorOptions{}, parse.ParserOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res != nil {
		defer res.Cleanup()
	}
	if pr == nil {
		t.Fatal("Dispatch returned nil ParseResult")
	}
	if pr.SourceFormat != "markdown" {
		t.Errorf("SourceFormat = %q, want %q", pr.SourceFormat, "markdown")
	}
	if pr.Root == nil {
		t.Fatal("ParseResult Root is nil")
	}
	if pr.Root.Type != parse.NodeRoot {
		t.Errorf("Root Type = %q, want %q", pr.Root.Type, parse.NodeRoot)
	}
}

// mockParser is a no-op Parser used for registry tests.
type mockParser struct{}

func (m *mockParser) SupportedExtensions() []string { return []string{".mock"} }
func (m *mockParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	return parse.NewParseResult(&parse.ResourceNode{Type: parse.NodeRoot}, res.Path, "mock", "mock"), nil
}
func (m *mockParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	return parse.NewParseResult(&parse.ResourceNode{Type: parse.NodeRoot}, sourcePath, "mock", "mock"), nil
}
