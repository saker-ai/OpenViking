package queuefs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisOptions verifies the option functions set the corresponding
// fields on the RedisServer.
func TestRedisOptions(t *testing.T) {
	srv := NewRedisServer("127.0.0.1:6379",
		WithRedisPassword("secret"),
		WithRedisDB(2),
	)
	assert.Equal(t, "secret", srv.password)
	assert.Equal(t, 2, srv.db)
	assert.Equal(t, "127.0.0.1:6379", srv.addr)
}

// TestRedisRegisterHandlerBeforeStart verifies RegisterHandler stores
// the handler when the server has not started yet (mux is nil).
func TestRedisRegisterHandlerBeforeStart(t *testing.T) {
	srv := NewRedisServer("127.0.0.1:0")
	h := func(_ context.Context, _ *Task) (*TaskResult, error) { return nil, nil }
	srv.RegisterHandler("test:kind", h)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, srv.handlers, "test:kind")
}

// TestRedisEnqueueNotStarted verifies Enqueue on an unstarted server
// returns the "not started" error when the task is valid.
func TestRedisEnqueueNotStarted(t *testing.T) {
	srv := NewRedisServer("127.0.0.1:0")
	err := srv.Enqueue(&Task{ID: "t1", Type: "test:kind", Payload: []byte("p")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}

// TestRedisShutdownWithoutStart verifies Shutdown on an unstarted server
// is a safe no-op.
func TestRedisShutdownWithoutStart(t *testing.T) {
	srv := NewRedisServer("127.0.0.1:0")
	// Should not panic or block.
	assert.NotPanics(t, func() {
		_ = srv.Shutdown()
	})
}
