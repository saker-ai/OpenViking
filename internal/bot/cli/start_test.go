package cli

import (
	"testing"
)

func TestNewStartCmd(t *testing.T) {
	cmd := NewStartCmd()
	if cmd == nil {
		t.Fatal("NewStartCmd returned nil")
	}
	if cmd.Use != "start" {
		t.Errorf("Use = %q, want %q", cmd.Use, "start")
	}
	if cmd.RunE == nil {
		t.Error("RunE is nil")
	}
	if cmd.Flags().Lookup("config") == nil {
		t.Error("config flag not registered")
	}
}

func TestNewChannelsCmd(t *testing.T) {
	cmd := NewChannelsCmd()
	if cmd == nil {
		t.Fatal("NewChannelsCmd returned nil")
	}
	if cmd.Use != "channels" {
		t.Errorf("Use = %q, want %q", cmd.Use, "channels")
	}
	if cmd.RunE == nil {
		t.Error("RunE is nil")
	}
}

func TestNewStatusCmd(t *testing.T) {
	cmd := NewStatusCmd()
	if cmd == nil {
		t.Fatal("NewStatusCmd returned nil")
	}
	if cmd.Use != "status" {
		t.Errorf("Use = %q, want %q", cmd.Use, "status")
	}
	if cmd.RunE == nil {
		t.Error("RunE is nil")
	}
}

func TestNewConsoleCmd(t *testing.T) {
	cmd := NewConsoleCmd()
	if cmd == nil {
		t.Fatal("NewConsoleCmd returned nil")
	}
	if cmd.Use != "console" {
		t.Errorf("Use = %q, want %q", cmd.Use, "console")
	}
	if cmd.RunE == nil {
		t.Error("RunE is nil")
	}
	if cmd.Flags().Lookup("config") == nil {
		t.Error("config flag not registered")
	}
}
