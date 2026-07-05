package version

import (
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	info := Get()
	if info.Version != "dev" {
		t.Errorf("default Version = %q, want %q", info.Version, "dev")
	}
	if info.GoVersion == "" {
		t.Error("GoVersion is empty")
	}
	if info.Platform == "" {
		t.Error("Platform is empty")
	}
	if !strings.Contains(info.Platform, "/") {
		t.Errorf("Platform %q must contain '/'", info.Platform)
	}
}

func TestString(t *testing.T) {
	s := String()
	if !strings.HasPrefix(s, "openviking ") {
		t.Errorf("String() = %q, want prefix %q", s, "openviking ")
	}
	// Must contain a platform segment like linux/amd64
	if !strings.Contains(s, "/") {
		t.Errorf("String() = %q must contain platform", s)
	}
}
