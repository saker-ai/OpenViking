// Package cron is the periodic-task scheduler for vikingbot. It wraps
// github.com/go-co-op/gocron/v2 so the bot can register housekeeping
// tasks (session cleanup, memory extraction, heartbeat, etc.) on a
// cron schedule.
package cron

import (
	"context"
	"fmt"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// Scheduler wraps a gocron.Scheduler. Tasks are registered before
// Start; Shutdown drains in-flight tasks.
type Scheduler struct {
	cfg   config.CronConfig
	sched gocron.Scheduler
}

// New constructs a Scheduler. It does not start the underlying
// gocron loop; call Start to begin scheduling.
func New(cfg config.CronConfig) (*Scheduler, error) {
	loc, err := time.LoadLocation(cfg.Location)
	if err != nil {
		loc = time.UTC
	}
	s, err := gocron.NewScheduler(gocron.WithLocation(loc))
	if err != nil {
		return nil, fmt.Errorf("cron: new scheduler: %w", err)
	}
	return &Scheduler{cfg: cfg, sched: s}, nil
}

// Schedule registers a function to run on a cron schedule. crontab is
// a standard 5-field cron expression (or 6-field with seconds when
// withSeconds is true). Returns an error if the schedule is invalid.
func (s *Scheduler) Schedule(name, crontab string, withSeconds bool, fn func(ctx context.Context)) error {
	_, err := s.sched.NewJob(
		gocron.CronJob(crontab, withSeconds),
		gocron.NewTask(func() {
			fn(context.Background())
		}),
	)
	if err != nil {
		return fmt.Errorf("cron: schedule %s: %w", name, err)
	}
	return nil
}

// Start launches the scheduler. It is idempotent: calling Start on an
// already-running scheduler is a no-op.
func (s *Scheduler) Start(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	s.sched.Start()
	return nil
}

// Shutdown stops the scheduler and waits for in-flight tasks.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	if err := s.sched.ShutdownWithContext(ctx); err != nil {
		return fmt.Errorf("cron: shutdown: %w", err)
	}
	return nil
}

// Jobs returns the registered jobs (for status reporting).
func (s *Scheduler) Jobs() []gocron.Job {
	return s.sched.Jobs()
}
