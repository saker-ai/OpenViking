package prompts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStore_LoadEmbedded(t *testing.T) {
	t.Parallel()
	s := NewStore("")
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	ids, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 43 {
		t.Fatalf("expected 43 embedded templates, got %d", len(ids))
	}
}

func TestStore_SourceEmbedded(t *testing.T) {
	t.Parallel()
	s := NewStore("")
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src := s.Source(); src != "embed" {
		t.Fatalf("Source=%q, want 'embed'", src)
	}
}

func TestStore_GetNotFound(t *testing.T) {
	t.Parallel()
	s := NewStore("")
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err := s.Get("does.not.exist")
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("expected ErrTemplateNotFound, got: %v", err)
	}
}

func TestStore_GetNotLoaded(t *testing.T) {
	t.Parallel()
	s := NewStore("")
	_, err := s.Get("vision.image_understanding")
	if !errors.Is(err, ErrStoreNotLoaded) {
		t.Fatalf("expected ErrStoreNotLoaded, got: %v", err)
	}
}

func TestStore_RenderStandardPrompt(t *testing.T) {
	t.Parallel()
	s := NewStore("")
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := s.Render("vision.image_understanding", map[string]any{
		"instruction": "describe the image",
		"context":     "user is asking about a chart",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "describe the image") {
		t.Fatalf("rendered output missing instruction: %q", out)
	}
	if !strings.Contains(out, "user is asking about a chart") {
		t.Fatalf("rendered output missing context: %q", out)
	}
}

func TestStore_RenderCompressionIfBranch(t *testing.T) {
	t.Parallel()
	s := NewStore("")
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := s.Render("compression.structured_summary", map[string]any{
		"latest_archive_overview": "PRIOR OVERVIEW",
		"messages":                "NEW MESSAGES",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "PRIOR OVERVIEW") {
		t.Fatalf("missing prior overview (if-branch should render): %q", out)
	}
	if !strings.Contains(out, "NEW MESSAGES") {
		t.Fatalf("missing messages: %q", out)
	}

	out, err = s.Render("compression.structured_summary", map[string]any{
		"messages": "ONLY MESSAGES",
	})
	if err != nil {
		t.Fatalf("Render without optional var: %v", err)
	}
	if strings.Contains(out, "Latest completed archive overview") {
		t.Fatalf("if-branch should be skipped when overview is empty: %q", out)
	}
	if !strings.Contains(out, "ONLY MESSAGES") {
		t.Fatalf("missing messages: %q", out)
	}
}

func TestStore_LoadDisk(t *testing.T) {
	t.Parallel()
	dir := writeTempPrompts(t)
	s := NewStore(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	ids, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 disk templates, got %d (%v)", len(ids), ids)
	}
	out, err := s.Render("greet.hello", map[string]any{"name": "Bob"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "Hello, Bob!"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestStore_ReloadFromDisk(t *testing.T) {
	t.Parallel()
	dir := writeTempPrompts(t)
	s := NewStore(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	before, err := s.Render("greet.hello", map[string]any{"name": "Bob"})
	if err != nil {
		t.Fatalf("Render before: %v", err)
	}
	if want := "Hello, Bob!"; before != want {
		t.Fatalf("before: got %q, want %q", before, want)
	}

	// Modify the YAML file on disk.
	helloPath := filepath.Join(dir, "greet", "hello.yaml")
	newContent := "metadata:\n  id: \"greet.hello\"\n  name: \"Hello\"\n  description: \"\"\n  version: \"1.0.0\"\n  language: \"en\"\n  category: \"greet\"\nvariables:\n  - name: name\n    type: string\n    required: true\ntemplate: |-\n  Hi there, {{ name }}!\n"
	if err := os.WriteFile(helloPath, []byte(newContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	count, err := s.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if count != 2 {
		t.Fatalf("Reload returned %d templates, want 2", count)
	}
	after, err := s.Render("greet.hello", map[string]any{"name": "Bob"})
	if err != nil {
		t.Fatalf("Render after: %v", err)
	}
	if want := "Hi there, Bob!"; after != want {
		t.Fatalf("after: got %q, want %q", after, want)
	}
	if s.ReloadCount() != 1 {
		t.Fatalf("ReloadCount=%d, want 1", s.ReloadCount())
	}
}

func TestStore_ReloadEmbedded(t *testing.T) {
	t.Parallel()
	s := NewStore("")
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	count, err := s.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if count != 43 {
		t.Fatalf("Reload returned %d, want 43", count)
	}
}

// writeTempPrompts writes a minimal templates directory layout to a
// temp dir and returns the dir path. Two templates: greet.hello and
// simple.echo.
func writeTempPrompts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"greet/hello.yaml": "metadata:\n  id: \"greet.hello\"\n  name: \"Hello\"\n  description: \"\"\n  version: \"1.0.0\"\n  language: \"en\"\n  category: \"greet\"\nvariables:\n  - name: name\n    type: string\n    required: true\ntemplate: |-\n  Hello, {{ name }}!\n",
		"simple/echo.yaml": "metadata:\n  id: \"simple.echo\"\n  name: \"Echo\"\n  description: \"\"\n  version: \"1.0.0\"\n  language: \"en\"\n  category: \"simple\"\nvariables:\n  - name: msg\n    type: string\n    required: true\ntemplate: |-\n  Echo: {{ msg }}\n",
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", full, err)
		}
	}
	return dir
}
