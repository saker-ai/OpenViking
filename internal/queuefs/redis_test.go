package queuefs

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// redisAddr returns the Redis address from OV_REDIS_ADDR or skips the test.
func redisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("OV_REDIS_ADDR")
	if addr == "" {
		t.Skip("set OV_REDIS_ADDR to run redis backend tests")
	}
	return addr
}

// TestRedisEnqueueAndProcess verifies basic enqueue/process against a live
// Redis. Gated on OV_REDIS_ADDR so CI without Redis skips.
func TestRedisEnqueueAndProcess(t *testing.T) {
	addr := redisAddr(t)

	var mu sync.Mutex
	var seen []string

	srv := NewRedisServer(addr)
	srv.RegisterHandler("test:redis:echo", func(ctx context.Context, task *Task) (*TaskResult, error) {
		mu.Lock()
		seen = append(seen, task.ID)
		mu.Unlock()
		return &TaskResult{}, nil
	})

	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()

	require.NoError(t, srv.Enqueue(&Task{
		ID:      "r1",
		Type:    "test:redis:echo",
		Payload: []byte("hello"),
	}))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 1
	}, 5*time.Second, 50*time.Millisecond, "handler did not run")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"r1"}, seen)
}

// TestRedisRetryOnFailure verifies asynq retries a failing task. Gated.
func TestRedisRetryOnFailure(t *testing.T) {
	addr := redisAddr(t)

	var attempts int32
	srv := NewRedisServer(addr)
	srv.RegisterHandler("test:redis:retry", func(ctx context.Context, task *Task) (*TaskResult, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			return nil, errors.New("transient")
		}
		return &TaskResult{}, nil
	})

	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()

	require.NoError(t, srv.Enqueue(&Task{
		ID:         "rr1",
		Type:       "test:redis:retry",
		MaxRetries: 3,
	}))

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&attempts) >= 2
	}, 10*time.Second, 100*time.Millisecond, "task did not retry to success")

	assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(2))
}

// TestRedisEnqueueValidation verifies basic validation without a live Redis.
func TestRedisEnqueueValidation(t *testing.T) {
	srv := NewRedisServer("127.0.0.1:0") // no live redis needed for validation
	// Without Start, client is nil.
	assert.Error(t, srv.Enqueue(nil))
	assert.Error(t, srv.Enqueue(&Task{Type: "t"})) // empty ID
	assert.Error(t, srv.Enqueue(&Task{ID: "x"}))   // empty type
}

// TestRedisStartTwiceErrors verifies Start rejects double-start. Gated
// because it needs a reachable Redis to actually start once.
func TestRedisStartTwiceErrors(t *testing.T) {
	addr := redisAddr(t)
	srv := NewRedisServer(addr)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()
	assert.Error(t, srv.Start(context.Background()))
}
