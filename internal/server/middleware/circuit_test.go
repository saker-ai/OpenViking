package middleware

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCircuitTripsAfterConsecutiveFailures(t *testing.T) {
	var mu sync.Mutex
	changes := []string{}
	cb := NewCircuitBreaker(CircuitConfig{
		Name:                   "test",
		MaxConsecutiveFailures: 3,
		Cooldown:               50 * time.Millisecond,
		HalfOpenMaxProbe:       1,
		OnStateChange: func(name string, from, to CircuitState) {
			mu.Lock()
			defer mu.Unlock()
			changes = append(changes, from.String()+"->"+to.String())
		},
	})

	// Three failures should trip the breaker.
	for i := 0; i < 3; i++ {
		_, err := Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
			return 0, errors.New("boom")
		})
		require.Error(t, err)
	}
	require.Equal(t, StateOpen, cb.State(), "breaker should be open after 3 failures")

	// Subsequent calls short-circuit with ErrCircuitOpen.
	_, err := Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
		t.Fatal("fn should not be called when open")
		return 0, nil
	})
	assert.ErrorIs(t, err, ErrCircuitOpen)

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, changes, "closed->open", "state transition callback should have fired")
}

func TestCircuitHalfOpenAndCloseOnSuccess(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		Name:                   "test",
		MaxConsecutiveFailures: 2,
		Cooldown:               20 * time.Millisecond,
		HalfOpenMaxProbe:       1,
	})

	// Trip the breaker.
	for i := 0; i < 2; i++ {
		_, _ = Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
			return 0, errors.New("boom")
		})
	}
	require.Equal(t, StateOpen, cb.State())

	// Wait for cooldown to expire so snapshotState transitions to half-open.
	time.Sleep(40 * time.Millisecond)
	require.Equal(t, StateHalfOpen, cb.State())

	// A successful probe should close the breaker.
	v, err := Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
		return 42, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 42, v)
	assert.Equal(t, StateClosed, cb.State())
}

func TestCircuitHalfOpenReopensOnFailure(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		Name:                   "test",
		MaxConsecutiveFailures: 2,
		Cooldown:               20 * time.Millisecond,
		HalfOpenMaxProbe:       1,
	})

	for i := 0; i < 2; i++ {
		_, _ = Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
			return 0, errors.New("boom")
		})
	}
	require.Equal(t, StateOpen, cb.State())
	time.Sleep(40 * time.Millisecond)
	require.Equal(t, StateHalfOpen, cb.State())

	// Failing probe re-opens immediately.
	_, err := Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
		return 0, errors.New("still broken")
	})
	require.Error(t, err)
	assert.Equal(t, StateOpen, cb.State())
}

func TestCircuitClosedResetsOnSuccess(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		Name:                   "test",
		MaxConsecutiveFailures: 3,
		Cooldown:               20 * time.Millisecond,
	})
	// Two failures then a success: success should reset the counter.
	_, _ = Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
		return 0, errors.New("boom")
	})
	_, _ = Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
		return 0, errors.New("boom")
	})
	_, _ = Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
		return 1, nil
	})
	require.Equal(t, StateClosed, cb.State())

	// Two more failures should not trip (consecutive count is now 2, not 4).
	_, _ = Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
		return 0, errors.New("boom")
	})
	_, _ = Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
		return 0, errors.New("boom")
	})
	assert.Equal(t, StateClosed, cb.State())
}

func TestCircuitNilBreakerPassthrough(t *testing.T) {
	var called bool
	v, err := Execute[int](nil, context.Background(), func(ctx context.Context) (int, error) {
		called = true
		return 7, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 7, v)
	assert.True(t, called)
}

func TestCircuitErrorRateTrip(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		Name:                   "test",
		MaxConsecutiveFailures: 100, // disable consecutive trip
		ErrorRateThreshold:     0.5,
		MinRequestsForRate:     4,
		Cooldown:               20 * time.Millisecond,
	})
	// Mix of success and failure; rate crosses 0.5 after the 4th sample.
	// fail, fail, ok, fail -> 3/4 = 0.75 -> trip on the 4th call.
	calls := []bool{true, true, false, true}
	for i, fail := range calls {
		_, _ = Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
			if fail {
				return 0, errors.New("boom")
			}
			return 1, nil
		})
		if i < 3 {
			assert.Equal(t, StateClosed, cb.State(), "should stay closed at sample %d", i+1)
		}
	}
	assert.Equal(t, StateOpen, cb.State(), "should trip after error rate exceeds threshold")
}

// TestCircuitConcurrentSafety hammers the breaker from multiple goroutines
// to surface any data race; run with -race to be meaningful.
func TestCircuitConcurrentSafety(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		Name:                   "test",
		MaxConsecutiveFailures: 50,
		Cooldown:               5 * time.Millisecond,
		HalfOpenMaxProbe:       1,
	})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = Execute[int](cb, context.Background(), func(ctx context.Context) (int, error) {
				if n%3 == 0 {
					return 0, errors.New("boom")
				}
				return 1, nil
			})
		}(i)
	}
	wg.Wait()
	// Just assert no panic / race; state is non-deterministic under load.
	_ = cb.State()
}
