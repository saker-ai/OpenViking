package parsers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/saker-ai/ctxhub/internal/parse"
)

func TestTextParser_Fixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.txt")
	content := "First paragraph.\n\nSecond paragraph.\n\nThird paragraph.\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := &TextParser{}
	res := &parse.LocalResource{Path: path, SourceType: parse.SourceLocal}
	pr, err := p.Parse(context.Background(), res, parse.ParserOptions{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
	if pr.SourceFormat != "text" {
		t.Errorf("SourceFormat = %q, want %q", pr.SourceFormat, "text")
	}
	if pr.ParserName != "TextParser" {
		t.Errorf("ParserName = %q, want %q", pr.ParserName, "TextParser")
	}
	// Three paragraphs split on blank lines.
	paraCount := 0
	for _, n := range pr.AllNodes() {
		if n.Type == parse.NodeParagraph {
			paraCount++
		}
	}
	if paraCount != 3 {
		t.Errorf("paragraph count = %d, want 3", paraCount)
	}
}

func TestTextParser_ParseContent(t *testing.T) {
	p := &TextParser{}
	pr, err := p.ParseContent(context.Background(), []byte("one\n\ntwo\n"), "test.txt", parse.ParserOptions{})
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
}

func TestTextParser_SingleLine(t *testing.T) {
	p := &TextParser{}
	pr, err := p.ParseContent(context.Background(), []byte("single line\n"), "one.txt", parse.ParserOptions{})
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
	paraCount := 0
	for _, n := range pr.AllNodes() {
		if n.Type == parse.NodeParagraph {
			paraCount++
		}
	}
	if paraCount != 1 {
		t.Errorf("paragraph count = %d, want 1", paraCount)
	}
}
