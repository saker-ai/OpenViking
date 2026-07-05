package parsers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/saker-ai/ctxhub/internal/parse"
)

func TestMarkdownParser_Fixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.md")
	content := `# Title

First paragraph.

## Section A

Second paragraph.

` + "```go\npackage main\nfunc main() {}\n```" + `

## Section B

Final paragraph.
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := &MarkdownParser{}
	res := &parse.LocalResource{Path: path, SourceType: parse.SourceLocal}
	pr, err := p.Parse(context.Background(), res, parse.ParserOptions{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
	if pr.SourceFormat != "markdown" {
		t.Errorf("SourceFormat = %q, want %q", pr.SourceFormat, "markdown")
	}
	if pr.ParserName != "MarkdownParser" {
		t.Errorf("ParserName = %q, want %q", pr.ParserName, "MarkdownParser")
	}
	// Should have root + sections + paragraphs + code.
	nodes := pr.AllNodes()
	sectionCount := 0
	paragraphCount := 0
	codeCount := 0
	for _, n := range nodes {
		switch n.Type {
		case parse.NodeSection:
			sectionCount++
		case parse.NodeParagraph:
			paragraphCount++
		case parse.NodeCode:
			codeCount++
		}
	}
	if sectionCount < 2 {
		t.Errorf("section count = %d, want >= 2", sectionCount)
	}
	if paragraphCount < 3 {
		t.Errorf("paragraph count = %d, want >= 3", paragraphCount)
	}
	if codeCount < 1 {
		t.Errorf("code count = %d, want >= 1", codeCount)
	}
}

func TestMarkdownParser_ParseContent(t *testing.T) {
	p := &MarkdownParser{}
	pr, err := p.ParseContent(context.Background(), []byte("# Hello\n\nWorld\n"), "test.md", parse.ParserOptions{})
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
	if pr.Root.Title != "test" {
		t.Errorf("Root Title = %q, want %q", pr.Root.Title, "test")
	}
}

func TestMarkdownParser_Empty(t *testing.T) {
	p := &MarkdownParser{}
	pr, err := p.ParseContent(context.Background(), []byte(""), "empty.md", parse.ParserOptions{})
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
}
