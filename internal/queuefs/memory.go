package queuefs

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryServer is an in-process, channel-based QueueServer. It maintains
// three priority queues (critical, default, low), retries failed tasks with
// exponential backoff, and surfaces permanent failures via a dead-letter
// slice and an optional hook.
//
// It is the default backend for tests and dev. It implements DeadLetterNotifier.
type MemoryServer struct {
	mu         sync.Mutex
	handlers   map[string]HandlerFunc
	started    bool
	shutdownCh chan struct{}
	wg         sync.WaitGroup

	criticalCh chan *taskAttempt
	defaultCh  chan *taskAttempt
	lowCh      chan *taskAttempt

	deadLetter     []deadTask
	deadLetterHook func(*Task, error)

	concurrency int
}

// taskAttempt is a single enqueue of a task, tracking the attempt number
// for retry-limit accounting.
type taskAttempt struct {
	task    *Task
	attempt int // 0-indexed; 0 is the first attempt
}

type deadTask struct {
	task     *Task
	err      error
	failedAt time.Time
}

// MemoryOption configures a MemoryServer at construction.
type MemoryOption func(*MemoryServer)

// WithConcurrency sets the worker goroutine count. Default is 8.
func WithConcurrency(n int) MemoryOption {
	return func(s *MemoryServer) {
		if n > 0 {
			s.concurrency = n
		}
	}
}

// WithDeadLetterHook installs a callback fired when a task exhausts its
// retries. The DAG scheduler uses this to call MarkFailed.
func WithDeadLetterHook(hook func(*Task, error)) MemoryOption {
	return func(s *MemoryServer) { s.deadLetterHook = hook }
}

// NewMemoryServer constructs an in-process queue server. Options are applied
// in order; later options win.
func NewMemoryServer(opts ...MemoryOption) *MemoryServer {
	s := &MemoryServer{
		handlers:    make(map[string]HandlerFunc),
		concurrency: 8,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// SetDeadLetterHook implements DeadLetterNotifier. It is safe to call
// before Start; the hook fires on future permanent failures only.
func (s *MemoryServer) SetDeadLetterHook(hook func(*Task, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadLetterHook = hook
}

// RegisterHandler binds a HandlerFunc to a task type. Re-registering a type
// replaces the prior handler. Safe to call before or after Start.
func (s *MemoryServer) RegisterHandler(taskType string, handler HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[taskType] = handler
}

// Enqueue adds a task to the queue selected by task.Priority. Returns an
// error if the server is not started or the task is invalid.
func (s *MemoryServer) Enqueue(task *Task) error {
	if task == nil {
		return fmt.Errorf("queuefs: nil task")
	}
	if task.ID == "" {
		return fmt.Errorf("queuefs: empty task id")
	}
	if task.Type == "" {
		return fmt.Errorf("queuefs: empty task type")
	}
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return fmt.Errorf("queuefs: server not started")
	}
	s.mu.Unlock()
	return s.enqueueAttempt(&taskAttempt{task: task, attempt: 0})
}

// Pending returns the total number of tasks waiting in the three priority
// channels. Best-effort: the count is a snapshot at the instant of the
// call and may change before the caller reads it. Used by /system/wait
// polling and observer endpoints.
func (s *MemoryServer) Pending() int {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return 0
	}
	s.mu.Unlock()
	return len(s.criticalCh) + len(s.defaultCh) + len(s.lowCh)
}

// Start launches worker goroutines. It is non-blocking and idempotent per
// instance (a second call returns an error). The ctx is currently unused
// beyond being stored for cancellation; workers exit on Shutdown.
func (s *MemoryServer) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("queuefs: already started")
	}
	s.shutdownCh = make(chan struct{})
	s.criticalCh = make(chan *taskAttempt, 256)
	s.defaultCh = make(chan *taskAttempt, 256)
	s.lowCh = make(chan *taskAttempt, 256)
	s.started = true
	s.mu.Unlock()

	for i := 0; i < s.concurrency; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return nil
}

// Shutdown signals workers to stop and blocks until they have drained.
// Idempotent.
func (s *MemoryServer) Shutdown() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	select {
	case <-s.shutdownCh:
		// already closing
	default:
		close(s.shutdownCh)
	}
	s.mu.Unlock()

	s.wg.Wait()

	s.mu.Lock()
	s.started = false
	s.mu.Unlock()
	return nil
}

// DeadLetter returns a snapshot of permanently failed tasks. Useful for
// tests asserting retry exhaustion.
func (s *MemoryServer) DeadLetter() []deadTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]deadTask, len(s.deadLetter))
	copy(out, s.deadLetter)
	return out
}

// enqueueAttempt pushes an attempt onto the appropriate priority queue. It
// blocks if the queue is full (backpressure) and silently drops the attempt
// if the server is shutting down.
func (s *MemoryServer) enqueueAttempt(a *taskAttempt) error {
	ch := s.queueFor(a.task.Priority)
	select {
	case ch <- a:
		return nil
	case <-s.shutdownCh:
		return nil
	}
}

func (s *MemoryServer) queueFor(p Priority) chan *taskAttempt {
	switch p {
	case PriorityCritical:
		return s.criticalCh
	case PriorityLow:
		return s.lowCh
	default:
		return s.defaultCh
	}
}

// worker drains the three priority queues with strict priority: critical is
// always drained before default before low. Within a priority, tasks are
// processed in FIFO order.
func (s *MemoryServer) worker() {
	defer s.wg.Done()
	for {
		// Drain critical first (non-blocking).
		select {
		case <-s.shutdownCh:
			return
		case a := <-s.criticalCh:
			s.process(a)
			continue
		default:
		}
		// Then default (non-blocking) — only consider low when default is empty.
		select {
		case <-s.shutdownCh:
			return
		case a := <-s.defaultCh:
			s.process(a)
			continue
		default:
		}
		// Then low (non-blocking).
		select {
		case <-s.shutdownCh:
			return
		case a := <-s.lowCh:
			s.process(a)
			continue
		default:
		}
		// All queues empty — block on any of the three. Go's select picks
		// randomly among ready cases, but at most one is ready here (we
		// drained all three above), so ordering stays correct.
		select {
		case <-s.shutdownCh:
			return
		case a := <-s.criticalCh:
			s.process(a)
		case a := <-s.defaultCh:
			s.process(a)
		case a := <-s.lowCh:
			s.process(a)
		}
	}
}

// process invokes the handler for a task attempt, recovering from panics.
// On failure it schedules a retry or dead-letters the task.
func (s *MemoryServer) process(a *taskAttempt) {
	defer func() {
		if r := recover(); r != nil {
			s.handleFailure(a, fmt.Errorf("queuefs: handler panic: %v", r))
		}
	}()

	s.mu.Lock()
	handler, ok := s.handlers[a.task.Type]
	s.mu.Unlock()

	if !ok {
		s.handleFailure(a, fmt.Errorf("queuefs: no handler for task type %q", a.task.Type))
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdownCh:
			cancel()
		}
	}()

	result, err := handler(ctx, a.task)
	if err != nil {
		s.handleFailure(a, err)
		return
	}
	_ = result
}

// handleFailure schedules a retry with exponential backoff if attempts
// remain, otherwise dead-letters the task and fires the dead-letter hook.
func (s *MemoryServer) handleFailure(a *taskAttempt, err error) {
	maxRetries := a.task.MaxRetries
	if a.attempt >= maxRetries {
		s.mu.Lock()
		s.deadLetter = append(s.deadLetter, deadTask{task: a.task, err: err, failedAt: time.Now()})
		hook := s.deadLetterHook
		s.mu.Unlock()
		if hook != nil {
			hook(a.task, err)
		}
		return
	}

	a.attempt++
	backoff := backoffDuration(a.attempt)
	time.AfterFunc(backoff, func() {
		select {
		case <-s.shutdownCh:
			return
		default:
			_ = s.enqueueAttempt(a)
		}
	})
}

// backoffDuration returns an exponential backoff: 20ms * 2^attempt, capped
// at 5 seconds. The minimum (attempt=1) is 40ms so tests stay snappy.
func backoffDuration(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt
	if shift > 7 {
		shift = 7
	}
	d := time.Duration(20*(1<<shift)) * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}
