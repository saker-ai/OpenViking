package cron

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

func TestNew_DefaultLocation(t *testing.T) {
	s, err := New(config.CronConfig{Enabled: true, Location: "BadLocation"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s == nil {
		t.Fatalf("nil scheduler")
	}
}

func TestNew_ValidLocation(t *testing.T) {
	s, err := New(config.CronConfig{Enabled: true, Location: "America/New_York"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = s
}

func TestScheduler_ScheduleInvalidCron(t *testing.T) {
	s, _ := New(config.CronConfig{Enabled: true})
	err := s.Schedule("bad", "not a cron expression", false, func(ctx context.Context) {})
	if err == nil {
		t.Fatalf("Schedule should error on invalid cron")
	}
}

func TestScheduler_StartAndShutdown(t *testing.T) {
	s, _ := New(config.CronConfig{Enabled: true, Location: "UTC"})
	// Use a 1-second interval to verify the task fires.
	var calls atomic.Int32
	if err := s.Schedule("tick", "* * * * * *", true, func(ctx context.Context) {
		calls.Add(1)
	}); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait long enough for at least one tick.
	time.Sleep(1500 * time.Millisecond)
	if calls.Load() == 0 {
		t.Errorf("task did not fire")
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestScheduler_DisabledDoesNotStart(t *testing.T) {
	s, _ := New(config.CronConfig{Enabled: false})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(s.Jobs()) != 0 {
		t.Errorf("jobs = %d, want 0", len(s.Jobs()))
	}
}
