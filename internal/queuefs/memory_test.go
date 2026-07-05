package queuefs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStartedMemory returns a started MemoryServer and a cancel func that
// shuts it down. Tests call this to reduce boilerplate.
func newStartedMemory(t *testing.T, opts ...MemoryOption) *MemoryServer {
	t.Helper()
	srv := NewMemoryServer(opts...)
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Shutdown() })
	return srv
}

// signalHandler returns a HandlerFunc that records the task ID on a slice
// (under mu) and returns nil. The slice is accessible for assertions.
func signalHandler(recorded *[]string, mu *sync.Mutex) HandlerFunc {
	return func(ctx context.Context, task *Task) (*TaskResult, error) {
		mu.Lock()
		*recorded = append(*recorded, task.ID)
		mu.Unlock()
		return &TaskResult{}, nil
	}
}

func TestMemoryEnqueueAndProcess(t *testing.T) {
	var recorded []string
	var mu sync.Mutex
	srv := newStartedMemory(t)
	srv.RegisterHandler("test:echo", signalHandler(&recorded, &mu))

	require.NoError(t, srv.Enqueue(&Task{
		ID:      "t1",
		Type:    "test:echo",
		Payload: []byte("hello"),
	}))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(recorded) == 1
	}, time.Second, 5*time.Millisecond, "handler did not run")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"t1"}, recorded)
}

func TestMemoryEnqueueValidation(t *testing.T) {
	srv := newStartedMemory(t)
	defer srv.Shutdown()

	// Nil task, empty ID, and empty type are rejected at enqueue time.
	assert.Error(t, srv.Enqueue(nil))
	assert.Error(t, srv.Enqueue(&Task{Type: "t"})) // empty ID
	assert.Error(t, srv.Enqueue(&Task{ID: "x"}))   // empty type

	// Valid shape but unregistered type: enqueue succeeds (the task is
	// accepted into the queue), then dead-letters at process time because
	// no handler is registered. That path is exercised in
	// TestMemoryNoHandlerFailsTask.
	assert.NoError(t, srv.Enqueue(&Task{ID: "x", Type: "t", MaxRetries: 0}))
}

func TestMemoryEnqueueBeforeStartFails(t *testing.T) {
	srv := NewMemoryServer()
	err := srv.Enqueue(&Task{ID: "x", Type: "t"})
	assert.Error(t, err)
}

func TestMemoryPriority(t *testing.T) {
	// Start with concurrency=0 so no worker drains between Enqueues;
	// we spawn a single worker after all three tasks are queued, making
	// the drain order deterministic. (Otherwise the worker can grab the
	// first-enqueued task before the higher-priority ones arrive.)
	var recorded []string
	var mu sync.Mutex
	srv := NewMemoryServer()
	srv.concurrency = 0 // no workers spawned in Start
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Shutdown() })
	srv.RegisterHandler("test:prio", func(ctx context.Context, task *Task) (*TaskResult, error) {
		mu.Lock()
		recorded = append(recorded, task.ID)
		mu.Unlock()
		return &TaskResult{}, nil
	})

	// Enqueue low, default, critical. With all three queued before the
	// worker starts, the priority drain yields critical -> default -> low.
	require.NoError(t, srv.Enqueue(&Task{ID: "low", Type: "test:prio", Priority: PriorityLow}))
	require.NoError(t, srv.Enqueue(&Task{ID: "default", Type: "test:prio", Priority: PriorityDefault}))
	require.NoError(t, srv.Enqueue(&Task{ID: "critical", Type: "test:prio", Priority: PriorityCritical}))

	// Spawn a single worker now that all three are queued.
	srv.wg.Add(1)
	go srv.worker()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(recorded) == 3
	}, time.Second, 5*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"critical", "default", "low"}, recorded)
}

func TestMemoryRetryOnFailure(t *testing.T) {
	var attempts int32
	srv := newStartedMemory(t)
	srv.RegisterHandler("test:retry", func(ctx context.Context, task *Task) (*TaskResult, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			return nil, errors.New("simulated transient failure")
		}
		return &TaskResult{}, nil
	})

	require.NoError(t, srv.Enqueue(&Task{
		ID:         "r1",
		Type:       "test:retry",
		MaxRetries: 5,
	}))

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&attempts) == 3
	}, 2*time.Second, 5*time.Millisecond, "handler did not reach 3 attempts")

	assert.Equal(t, int32(3), atomic.LoadInt32(&attempts))
	// No dead-letter entries since it eventually succeeded.
	assert.Empty(t, srv.DeadLetter())
}

func TestMemoryDeadLetterOnExhaustion(t *testing.T) {
	var attempts int32
	hookCalls := int32(0)
	srv := newStartedMemory(t, WithDeadLetterHook(func(*Task, error) {
		atomic.AddInt32(&hookCalls, 1)
	}))
	srv.RegisterHandler("test:dlq", func(ctx context.Context, task *Task) (*TaskResult, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, errors.New("permanent failure")
	})

	require.NoError(t, srv.Enqueue(&Task{
		ID:         "d1",
		Type:       "test:dlq",
		MaxRetries: 2,
	}))

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&attempts) == 3 // initial + 2 retries
	}, 2*time.Second, 5*time.Millisecond, "did not exhaust retries")

	assert.Equal(t, int32(3), atomic.LoadInt32(&attempts))
	require.Len(t, srv.DeadLetter(), 1, "one dead-letter entry expected")
	assert.Equal(t, "d1", srv.DeadLetter()[0].task.ID)
	assert.Equal(t, int32(1), atomic.LoadInt32(&hookCalls), "dead-letter hook should fire once")
}

func TestMemoryZeroRetriesDeadLettersImmediately(t *testing.T) {
	var attempts int32
	srv := newStartedMemory(t)
	srv.RegisterHandler("test:zero", func(ctx context.Context, task *Task) (*TaskResult, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, errors.New("fail")
	})

	require.NoError(t, srv.Enqueue(&Task{
		ID:         "z1",
		Type:       "test:zero",
		MaxRetries: 0,
	}))

	require.Eventually(t, func() bool {
		return len(srv.DeadLetter()) == 1
	}, time.Second, 5*time.Millisecond)

	assert.Equal(t, int32(1), atomic.LoadInt32(&attempts))
}

func TestMemoryShutdownIsIdempotent(t *testing.T) {
	srv := NewMemoryServer()
	require.NoError(t, srv.Start(context.Background()))
	require.NoError(t, srv.Shutdown())
	assert.NoError(t, srv.Shutdown())
}

func TestMemoryStartTwiceErrors(t *testing.T) {
	srv := NewMemoryServer()
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()
	assert.Error(t, srv.Start(context.Background()))
}

func TestMemoryNoHandlerFailsTask(t *testing.T) {
	srv := newStartedMemory(t)
	require.NoError(t, srv.Enqueue(&Task{
		ID:         "nh1",
		Type:       "test:unregistered",
		MaxRetries: 1,
	}))

	require.Eventually(t, func() bool {
		return len(srv.DeadLetter()) == 1
	}, 2*time.Second, 5*time.Millisecond, "missing handler should dead-letter the task")
}

func TestMemoryHandlerPanicRetried(t *testing.T) {
	var attempts int32
	srv := newStartedMemory(t)
	srv.RegisterHandler("test:panic", func(ctx context.Context, task *Task) (*TaskResult, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			panic("boom")
		}
		return &TaskResult{}, nil
	})

	require.NoError(t, srv.Enqueue(&Task{
		ID:         "p1",
		Type:       "test:panic",
		MaxRetries: 3,
	}))

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&attempts) == 2
	}, 2*time.Second, 5*time.Millisecond)

	assert.Equal(t, int32(2), atomic.LoadInt32(&attempts))
	assert.Empty(t, srv.DeadLetter())
}

func TestPriorityQueueName(t *testing.T) {
	assert.Equal(t, "critical", PriorityCritical.QueueName())
	assert.Equal(t, "default", PriorityDefault.QueueName())
	assert.Equal(t, "low", PriorityLow.QueueName())
}
