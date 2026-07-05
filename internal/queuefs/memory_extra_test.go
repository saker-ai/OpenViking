package queuefs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBackoffDuration verifies the exponential backoff formula and caps.
func TestBackoffDuration(t *testing.T) {
	// attempt < 1 clamps to 1 (40ms).
	assert.Equal(t, 40*time.Millisecond, backoffDuration(0))
	assert.Equal(t, 40*time.Millisecond, backoffDuration(-5))
	// attempt = 1 → 40ms; attempt = 2 → 80ms; attempt = 3 → 160ms.
	assert.Equal(t, 40*time.Millisecond, backoffDuration(1))
	assert.Equal(t, 80*time.Millisecond, backoffDuration(2))
	assert.Equal(t, 160*time.Millisecond, backoffDuration(3))
	// shift caps at 7 (attempt=8 → 20*128=2560ms; attempt=10 same).
	assert.Equal(t, backoffDuration(8), backoffDuration(10))
	// large attempt also hits the shift cap (2560ms).
	assert.Equal(t, 2560*time.Millisecond, backoffDuration(20))
}

// TestMemoryServerDeadLetterHook verifies the dead-letter hook fires when
// a task exhausts its retries.
func TestMemoryServerDeadLetterHook(t *testing.T) {
	srv := NewMemoryServer()
	var deadLettered int64
	srv.deadLetterHook = func(_ *Task, _ error) {
		atomic.AddInt64(&deadLettered, 1)
	}
	srv.RegisterHandler("dlq:kind", func(_ context.Context, _ *Task) (*TaskResult, error) {
		return nil, context.DeadlineExceeded
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, srv.Start(ctx))

	task := &Task{ID: "dlq1", Type: "dlq:kind", Payload: []byte("p"), MaxRetries: 1}
	require.NoError(t, srv.Enqueue(task))
	// Wait for the task to be processed and dead-lettered.
	require.Eventually(t, func() bool {
		return atomic.LoadInt64(&deadLettered) > 0
	}, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, srv.Shutdown())
}

// TestMemoryServerEnqueueNotStarted verifies Enqueue on an unstarted server
// returns the "not started" error.
func TestMemoryServerEnqueueNotStarted(t *testing.T) {
	srv := NewMemoryServer()
	err := srv.Enqueue(&Task{ID: "t1", Type: "k", Payload: []byte("p")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}

// TestMemoryServerEnqueueValidation verifies Enqueue validates the task.
func TestMemoryServerEnqueueValidation(t *testing.T) {
	srv := NewMemoryServer()
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()

	assert.Contains(t, srv.Enqueue(nil).Error(), "nil task")
	assert.Contains(t, srv.Enqueue(&Task{Type: "k"}).Error(), "empty task id")
	assert.Contains(t, srv.Enqueue(&Task{ID: "x"}).Error(), "empty task type")
}

// TestTopologicalSort_NilTask verifies topologicalSort rejects a nil task
// entry in the slice.
func TestTopologicalSort_NilTask(t *testing.T) {
	_, err := topologicalSort([]*Task{nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil task in dag")
}
