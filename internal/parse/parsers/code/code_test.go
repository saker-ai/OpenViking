package code

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/saker-ai/ctxhub/internal/parse"
)

func TestCodeParser_PythonFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.py")
	content := `package main

import os

def greet(name):
    print(f"Hello, {name}")

class Foo:
    def __init__(self):
        self.x = 1
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	factory, ok := parse.LookupParserForExt(".py")
	if !ok {
		t.Skip("python parser not registered")
	}
	p, err := factory()
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	res := &parse.LocalResource{Path: path, SourceType: parse.SourceLocal}
	pr, err := p.Parse(context.Background(), res, parse.ParserOptions{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
	if pr.SourceFormat != "python" {
		t.Errorf("SourceFormat = %q, want %q", pr.SourceFormat, "python")
	}
	// Should have at least one code node.
	codeCount := 0
	for _, n := range pr.AllNodes() {
		if n.Type == parse.NodeCode {
			codeCount++
		}
	}
	if codeCount < 1 {
		t.Errorf("code node count = %d, want >= 1", codeCount)
	}
}

func TestCodeParser_JavaScriptFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.js")
	content := `function greet(name) {
    console.log("Hello, " + name);
}

const x = 42;
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	factory, ok := parse.LookupParserForExt(".js")
	if !ok {
		t.Skip("javascript parser not registered")
	}
	p, err := factory()
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	res := &parse.LocalResource{Path: path, SourceType: parse.SourceLocal}
	pr, err := p.Parse(context.Background(), res, parse.ParserOptions{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
}

func TestCodeParser_ParseContent(t *testing.T) {
	factory, ok := parse.LookupParserForExt(".go")
	if !ok {
		t.Skip("golang parser not registered")
	}
	p, err := factory()
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	pr, err := p.ParseContent(context.Background(), []byte("package main\nfunc main() {}\n"), "test.go", parse.ParserOptions{})
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if pr == nil || pr.Root == nil {
		t.Fatal("ParseResult or Root is nil")
	}
}
