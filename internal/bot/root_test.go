package bot

import (
	"testing"
)

func TestNewRoot_RegistersSubcommands(t *testing.T) {
	root := NewRoot()
	if root == nil {
		t.Fatal("NewRoot returned nil")
	}
	if root.Use != "vikingbot" {
		t.Errorf("Use = %q, want %q", root.Use, "vikingbot")
	}
	want := []string{"version", "start", "channels", "status", "console"}
	got := map[string]bool{}
	for _, c := range root.Commands() {
		got[c.Use] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing subcommand %q; have %v", w, got)
		}
	}
}
