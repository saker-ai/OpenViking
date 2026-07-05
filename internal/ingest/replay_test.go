package ingest

import (
	"strings"
	"testing"
)

func TestOVSessionID(t *testing.T) {
	cases := []struct {
		name     string
		harness  string
		nativeID string
		want     string
	}{
		{"simple", "claude_code", "abc123", "claude_code__abc123"},
		{"empty harness", "", "abc123", "harness__abc123"},
		{"empty native", "claude_code", "", "claude_code__unknown"},
		{"both empty", "", "", "harness__unknown"},
		{"special chars in native", "claude_code", "abc/def:ghi", "claude_code__abc-def-ghi"},
		{"special chars in harness", "open-claw!", "session1", "open-claw__session1"},
		{"collapses dashes", "claude_code", "a---b", "claude_code__a-b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := OVSessionID(tc.harness, tc.nativeID)
			if got != tc.want {
				t.Errorf("OVSessionID(%q, %q) = %q, want %q",
					tc.harness, tc.nativeID, got, tc.want)
			}
		})
	}
}

func TestOVSessionID_ContainsPeerSep(t *testing.T) {
	id := OVSessionID("claude_code", "session-1")
	if !strings.Contains(id, PeerSep) {
		t.Errorf("OVSessionID %q does not contain PeerSep %q", id, PeerSep)
	}
}

func TestOVSessionID_Stable(t *testing.T) {
	a := OVSessionID("claude_code", "session-1")
	b := OVSessionID("claude_code", "session-1")
	if a != b {
		t.Errorf("OVSessionID not stable: %q vs %q", a, b)
	}
}
