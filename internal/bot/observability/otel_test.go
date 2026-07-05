package observability

import (
	"context"
	"errors"
	"testing"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

func TestNew_Defaults(t *testing.T) {
	b := New(config.OTELConfig{ServiceName: "vikingbot", LogLevel: "info"})
	if b == nil {
		t.Fatalf("nil bot")
	}
	if b.Tracer() == nil {
		t.Errorf("tracer is nil")
	}
	if b.Logger() == nil {
		t.Errorf("logger is nil")
	}
}

func TestBot_SpanReturnsEndFunc(t *testing.T) {
	b := New(config.OTELConfig{ServiceName: "vikingbot"})
	ctx, end := b.Span(context.Background(), "test-op")
	if ctx == nil {
		t.Errorf("ctx is nil")
	}
	if end == nil {
		t.Fatalf("end is nil")
	}
	end(nil)
	end(errors.New("test error")) // should not panic
}

func TestBot_SetLogger(t *testing.T) {
	b := New(config.OTELConfig{ServiceName: "v"})
	b.SetLogger(nil) // should not crash, falls back to default
	if b.Logger() == nil {
		t.Errorf("Logger is nil after SetLogger(nil)")
	}
}

func TestToServerOTEL_MapsFields(t *testing.T) {
	got := toServerOTEL(config.OTELConfig{
		Exporter:    "memory",
		ServiceName: "vikingbot",
		SampleRate:  0.5,
		LogLevel:    "debug",
	})
	if got.Exporter != "memory" || got.ServiceName != "vikingbot" || got.SampleRate != 0.5 || got.LogLevel != "debug" {
		t.Errorf("mapped = %+v", got)
	}
}
