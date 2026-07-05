package parsers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/saker-ai/ctxhub/internal/parse"
)

func TestHTMLParser_Fixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.html")
	content := `<!DOCTYPE html>
<html>
<head><title>Test Page</title></head>
<body>
<article>
<h1>Main Heading</h1>
<p>First paragraph of content.</p>
<p>Second paragraph of content.</p>
<pre><code>code block</code></pre>
</article>
</body>
</html>`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := &HTMLParser{}
	res := &parse.LocalResource{Path: path, SourceType: parse.SourceLocal}
	pr, err := p.Parse(context.Background(), res, parse.ParserOptions{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
	if pr.SourceFormat != "html" {
		t.Errorf("SourceFormat = %q, want %q", pr.SourceFormat, "html")
	}
	if pr.ParserName != "HTMLParser" {
		t.Errorf("ParserName = %q, want %q", pr.ParserName, "HTMLParser")
	}
	// Should have at least some content nodes.
	nodeCount := len(pr.AllNodes())
	if nodeCount < 2 {
		t.Errorf("node count = %d, want >= 2", nodeCount)
	}
}

func TestHTMLParser_ParseContent(t *testing.T) {
	p := &HTMLParser{}
	html := []byte("<html><body><p>Hello world</p></body></html>")
	pr, err := p.ParseContent(context.Background(), html, "test.html", parse.ParserOptions{})
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
}

func TestHTMLParser_Empty(t *testing.T) {
	p := &HTMLParser{}
	pr, err := p.ParseContent(context.Background(), []byte(""), "empty.html", parse.ParserOptions{})
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
}
