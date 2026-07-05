package ingest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	gocron "github.com/go-co-op/gocron/v2"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// PollerOptions configures a Poller.
type PollerOptions struct {
	// PollInterval is the fallback interval for sources without an
	// explicit per-source interval. Defaults to 5s.
	PollInterval time.Duration
	// WatchPaths enables fsnotify watches on the listed local paths.
	// File-change events trigger an immediate backfill of the affected
	// source.
	WatchPaths []string
	// CronSchedule is a 5-field cron expression for periodic backfills.
	// Empty disables cron.
	CronSchedule string
	// CronTimezone is the timezone for the cron expression (defaults to
	// UTC).
	CronTimezone string
}

// Poller watches configured sources for new conversation logs and submits
// backfill jobs. It mirrors openviking/ingest/poller.IngestPoller but
// swaps apscheduler for fsnotify + gocron/v2, and submits to queuefs
// (asynq or memory) via the Enqueuer interface.
//
// The Poller is goroutine-safe: Start launches the watch and cron
// goroutines, Stop joins them.
type Poller struct {
	mu      sync.Mutex
	sources map[string]Source
	orch    *Orchestrator
	opts    PollerOptions
	watcher *fsnotify.Watcher
	cron    gocron.Scheduler
	stopCh  chan struct{}
	started bool
	stopped bool
}

// Enqueuer is the minimal queue surface the Poller needs. It is satisfied
// by queuefs.QueueServer (memory or redis).
type Enqueuer interface {
	Enqueue(task any) error
}

// NewPoller constructs a Poller. orch may be nil; callers that don't need
// the full orchestrator can implement OnEvent directly.
func NewPoller(orch *Orchestrator, sources map[string]Source, opts PollerOptions) *Poller {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if sources == nil {
		sources = map[string]Source{}
	}
	return &Poller{
		sources: sources,
		orch:    orch,
		opts:    opts,
		stopCh:  make(chan struct{}),
	}
}

// Register attaches a source under name. Must be called before Start.
func (p *Poller) Register(name string, src Source) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sources[name] = src
}

// Start launches the watch + cron goroutines. Returns an error when
// fsnotify or gocron fails to initialize. Start is idempotent: a second
// call returns nil without re-launching.
func (p *Poller) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return nil
	}
	p.started = true
	p.mu.Unlock()

	if len(p.opts.WatchPaths) > 0 {
		w, err := fsnotify.NewWatcher()
		if err != nil {
			return domain.Wrap(domain.CodeInternalError, 500, err)
		}
		for _, path := range p.opts.WatchPaths {
			if err := w.Add(path); err != nil {
				w.Close()
				return domain.Wrap(domain.CodeInternalError, 500,
					errors.Join(errors.New("ingest: watch "+path), err))
			}
		}
		p.watcher = w
		go p.watchLoop(ctx)
	}

	if p.opts.CronSchedule != "" {
		loc, err := time.LoadLocation(p.opts.CronTimezone)
		if err != nil {
			loc = time.UTC
		}
		s, err := gocron.NewScheduler(gocron.WithLocation(loc))
		if err != nil {
			p.Stop()
			return domain.Wrap(domain.CodeInternalError, 500, err)
		}
		_, err = s.NewJob(gocron.CronJob(p.opts.CronSchedule, true),
			gocron.NewTask(func() {
				p.tickAll(ctx)
			}))
		if err != nil {
			s.Shutdown()
			p.Stop()
			return domain.Wrap(domain.CodeInternalError, 500, err)
		}
		p.cron = s
		s.Start()
	}

	// Always run a polling goroutine as a self-healing fallback (a missed
	// fsnotify event or a missed cron tick just means the next poll reads
	// cursor->EOF).
	go p.pollLoop(ctx)
	return nil
}

// Stop joins the watch + cron goroutines. Idempotent.
func (p *Poller) Stop() error {
	p.mu.Lock()
	if !p.started || p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()

	close(p.stopCh)
	if p.watcher != nil {
		_ = p.watcher.Close()
	}
	if p.cron != nil {
		_ = p.cron.Shutdown()
	}
	return nil
}

// watchLoop forwards fsnotify events to tickAll. A single write/create
// event triggers a backfill pass; the loop is throttled by the consumer
// reading the events channel (no extra debounce needed because the
// orchestrator's cursor makes backfill idempotent).
func (p *Poller) watchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case event, ok := <-p.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				p.tickAll(ctx)
			}
		case _, ok := <-p.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// pollLoop is the periodic fallback. It ticks every opts.PollInterval
// until stopped.
func (p *Poller) pollLoop(ctx context.Context) {
	t := time.NewTicker(p.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-t.C:
			p.tickAll(ctx)
		}
	}
}

// tickAll runs a single backfill pass for every registered source. It is
// the goroutine-equivalent of Python's IngestPoller._tick.
func (p *Poller) tickAll(ctx context.Context) {
	p.mu.Lock()
	names := make([]string, 0, len(p.sources))
	for n := range p.sources {
		names = append(names, n)
	}
	p.mu.Unlock()

	if p.orch == nil {
		return
	}
	for _, name := range names {
		p.mu.Lock()
		src := p.sources[name]
		p.mu.Unlock()
		if src == nil {
			continue
		}
		_ = p.orch.BackfillSource(ctx, name, src, BackfillOptions{})
	}
}
