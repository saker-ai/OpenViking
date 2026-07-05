package ingest

import (
	"strings"
	"testing"
)

func TestSafePeerID(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"ascii alnum", "alice", "alice"},
		{"ascii with email", "alice@example.com", "alice@example.com"},
		{"ascii with dashes", "alice-bob", "alice-bob"},
		{"collapses dashes", "alice---bob", "alice-bob"},
		{"trims leading dash", "-alice", "alice"},
		{"trims trailing dash", "alice-", "alice"},
		// Inputs with non-alnum ASCII chars (like /) are base64-encoded.
		{"disallowed chars encoded", "alice/bob", "ext-YWxpY2UvYm9i"},
		// Non-ASCII inputs are base64-encoded.
		{"non-ascii encoded", "张三", "ext-5byg5LiJ"},
		{"mixed ascii nonascii", "alice张", "ext-YWxpY2XlvKA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SafePeerID(tc.raw)
			if got != tc.want {
				t.Errorf("SafePeerID(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSafePeerID_Stable(t *testing.T) {
	// Same input always produces same output.
	a := SafePeerID("test-user-1")
	b := SafePeerID("test-user-1")
	if a != b {
		t.Errorf("SafePeerID not stable: %q vs %q", a, b)
	}
}

func TestAssistantPeerID(t *testing.T) {
	cases := []struct {
		name     string
		harness  string
		model    string
		provider string
		want     string
	}{
		{"no provider", "claude_code", "claude-sonnet-4", "", "claude_code__claude-sonnet-4"},
		{"with provider", "claude_code", "claude-sonnet-4", "anthropic", "claude_code__anthropic__claude-sonnet-4"},
		{"empty model", "codex", "", "", "codex__unknown"},
		{"empty harness", "", "gpt-4", "", "unknown__gpt-4"},
		{"special chars sanitized", "open-claw", "gpt-4/mini", "openai", "open-claw__openai__gpt-4-mini"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AssistantPeerID(tc.harness, tc.model, tc.provider)
			if got != tc.want {
				t.Errorf("AssistantPeerID(%q, %q, %q) = %q, want %q",
					tc.harness, tc.model, tc.provider, got, tc.want)
			}
		})
	}
}

func TestPeerID(t *testing.T) {
	a := PeerID("account-1", "session-abc")
	if a == "" {
		t.Fatal("PeerID returned empty")
	}
	// Should be deterministic.
	b := PeerID("account-1", "session-abc")
	if a != b {
		t.Errorf("PeerID not deterministic: %q vs %q", a, b)
	}
	// Different inputs should produce different outputs.
	c := PeerID("account-1", "session-xyz")
	if a == c {
		t.Errorf("PeerID collision for different sourceIDs: %q == %q", a, c)
	}
	// Empty sourceID falls back to SafePeerID(account).
	d := PeerID("alice", "")
	if d != SafePeerID("alice") {
		t.Errorf("PeerID with empty sourceID = %q, want %q", d, SafePeerID("alice"))
	}
}

func TestResolveGitHumanPeer_Fallback(t *testing.T) {
	// Non-existent cwd should fall back to SafePeerID(fallback).
	got := ResolveGitHumanPeer("/nonexistent/path/that/does/not/exist", "fallback-user")
	want := SafePeerID("fallback-user")
	if got != want {
		t.Errorf("ResolveGitHumanPeer(nonexistent) = %q, want %q", got, want)
	}
}

func TestResolveGitHumanPeer_EmptyCWD(t *testing.T) {
	got := ResolveGitHumanPeer("", "fallback-user")
	want := SafePeerID("fallback-user")
	if got != want {
		t.Errorf("ResolveGitHumanPeer(empty cwd) = %q, want %q", got, want)
	}
}

func TestPeerIDComponents_NoPeerSep(t *testing.T) {
	// Assistant peer IDs must contain exactly the right number of separators.
	id := AssistantPeerID("claude_code", "claude-sonnet-4", "")
	sepCount := strings.Count(id, PeerSep)
	if sepCount != 1 {
		t.Errorf("AssistantPeerID without provider has %d separators, want 1", sepCount)
	}
	id = AssistantPeerID("claude_code", "claude-sonnet-4", "anthropic")
	sepCount = strings.Count(id, PeerSep)
	if sepCount != 2 {
		t.Errorf("AssistantPeerID with provider has %d separators, want 2", sepCount)
	}
}
