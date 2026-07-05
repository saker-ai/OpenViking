package prompts

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatcher_HotReloadOnWrite(t *testing.T) {
	t.Parallel()
	dir := writeTempPrompts(t)
	s := NewStore(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	w, err := NewWatcher(s)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()
	w.SetDebounce(50 * time.Millisecond)

	var reloads atomic.Int64
	ready := make(chan struct{}, 8)
	w.SetOnReload(func() {
		reloads.Add(1)
		select {
		case ready <- struct{}{}:
		default:
		}
	})

	before, err := s.Render("greet.hello", map[string]any{"name": "Bob"})
	if err != nil {
		t.Fatalf("Render before: %v", err)
	}
	if want := "Hello, Bob!"; before != want {
		t.Fatalf("before: got %q, want %q", before, want)
	}

	helloPath := filepath.Join(dir, "greet", "hello.yaml")
	newContent := "metadata:\n  id: \"greet.hello\"\n  name: \"Hello\"\n  description: \"\"\n  version: \"1.0.0\"\n  language: \"en\"\n  category: \"greet\"\nvariables:\n  - name: name\n    type: string\n    required: true\ntemplate: |-\n  Hi there, {{ name }}!\n"
	if err := os.WriteFile(helloPath, []byte(newContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatalf("watcher did not fire Reload within 3s (reloads=%d)", reloads.Load())
	}

	// Give the Reload a moment to swap the snapshot. The OnReload
	// callback fires after Reload returns, so the snapshot is already
	// updated when we reach here.
	after, err := s.Render("greet.hello", map[string]any{"name": "Bob"})
	if err != nil {
		t.Fatalf("Render after: %v", err)
	}
	if want := "Hi there, Bob!"; after != want {
		t.Fatalf("after: got %q, want %q", after, want)
	}
	if c := reloads.Load(); c < 1 {
		t.Fatalf("reloads=%d, want >= 1", c)
	}
}

func TestWatcher_HotReloadOnCreate(t *testing.T) {
	t.Parallel()
	dir := writeTempPrompts(t)
	s := NewStore(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	w, err := NewWatcher(s)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()
	w.SetDebounce(50 * time.Millisecond)

	ready := make(chan struct{}, 8)
	w.SetOnReload(func() {
		select {
		case ready <- struct{}{}:
		default:
		}
	})

	// Create a new template file.
	newPath := filepath.Join(dir, "greet", "yo.yaml")
	newContent := "metadata:\n  id: \"greet.yo\"\n  name: \"Yo\"\n  description: \"\"\n  version: \"1.0.0\"\n  language: \"en\"\n  category: \"greet\"\nvariables:\n  - name: name\n    type: string\n    required: true\ntemplate: |-\n  Yo, {{ name }}!\n"
	if err := os.WriteFile(newPath, []byte(newContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatalf("watcher did not fire Reload for create")
	}

	out, err := s.Render("greet.yo", map[string]any{"name": "Sam"})
	if err != nil {
		t.Fatalf("Render new template: %v", err)
	}
	if want := "Yo, Sam!"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestWatcher_RejectsEmbedOnly(t *testing.T) {
	t.Parallel()
	s := NewStore("")
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err := NewWatcher(s)
	if err == nil {
		t.Fatal("expected error watching embedded archive, got nil")
	}
}

func TestWatcher_StopIdempotent(t *testing.T) {
	t.Parallel()
	dir := writeTempPrompts(t)
	s := NewStore(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	w, err := NewWatcher(s)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.Stop()
	w.Stop() // must not panic
}
