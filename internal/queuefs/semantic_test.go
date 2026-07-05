package queuefs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSemanticStore returns a fresh in-memory SemanticStore. Each call uses
// a separate shared-cache :memory: DSN so tests do not share state.
func newSemanticStore(t *testing.T) *SemanticStore {
	t.Helper()
	// Use a unique shared-cache DB name per test so concurrent tests do
	// not collide on the same in-memory database.
	dsn := fmt.Sprintf("file:semtest_%d?mode=memory&cache=shared&_txlock=immediate", time.Now().UnixNano())
	s, err := NewSemanticStore(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSemanticEnqueueAndClaim(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{
		ID:       "t1",
		Type:     "test:echo",
		Payload:  []byte("hello"),
		Priority: PriorityDefault,
	}, WithAccount("acct-A")))

	lease, err := store.Claim(ctx, "acct-A")
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, "t1", lease.TaskID)
	assert.Equal(t, "acct-A", lease.Account)
	assert.Equal(t, "test:echo", lease.Task.Type)
	assert.Equal(t, []byte("hello"), lease.Task.Payload)
	assert.False(t, lease.ExpiresAt.IsZero())

	// No more pending tasks for acct-A.
	lease2, err := store.Claim(ctx, "acct-A")
	require.NoError(t, err)
	assert.Nil(t, lease2, "no second task expected")
}

func TestSemanticPriorityOrdering(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	// Enqueue low, default, critical out of order. Claim should return
	// critical -> default -> low (priority DESC, FIFO within priority).
	require.NoError(t, store.Enqueue(ctx, &Task{ID: "low", Type: "t", Priority: PriorityLow}, WithAccount("a")))
	require.NoError(t, store.Enqueue(ctx, &Task{ID: "critical", Type: "t", Priority: PriorityCritical}, WithAccount("a")))
	require.NoError(t, store.Enqueue(ctx, &Task{ID: "default", Type: "t", Priority: PriorityDefault}, WithAccount("a")))

	var order []string
	for i := 0; i < 3; i++ {
		lease, err := store.Claim(ctx, "a")
		require.NoError(t, err)
		require.NotNil(t, lease)
		order = append(order, lease.TaskID)
		require.NoError(t, store.Complete(ctx, lease.ID))
	}
	assert.Equal(t, []string{"critical", "default", "low"}, order)
}

func TestSemanticFIFOWithinPriority(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	// Enqueue three same-priority tasks; claim order must match enqueue
	// order. Tiny sleeps guard against RFC3339Nano sub-ms collisions.
	for i, id := range []string{"fifo1", "fifo2", "fifo3"} {
		require.NoError(t, store.Enqueue(ctx, &Task{ID: id, Type: "t", Priority: PriorityDefault}, WithAccount("a")))
		if i < 2 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	var order []string
	for i := 0; i < 3; i++ {
		lease, err := store.Claim(ctx, "a")
		require.NoError(t, err)
		require.NotNil(t, lease)
		order = append(order, lease.TaskID)
		require.NoError(t, store.Complete(ctx, lease.ID))
	}
	assert.Equal(t, []string{"fifo1", "fifo2", "fifo3"}, order)
}

func TestSemanticAccountIsolation(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "a-task", Type: "t", Priority: PriorityCritical}, WithAccount("acct-A")))
	require.NoError(t, store.Enqueue(ctx, &Task{ID: "b-task", Type: "t", Priority: PriorityLow}, WithAccount("acct-B")))

	// acct-A's Claim returns only a-task; b-task must be invisible.
	lease, err := store.Claim(ctx, "acct-A")
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, "a-task", lease.TaskID)
	require.NoError(t, store.Complete(ctx, lease.ID))

	lease, err = store.Claim(ctx, "acct-A")
	require.NoError(t, err)
	assert.Nil(t, lease, "acct-A must not see acct-B's task")

	// acct-B sees its own task.
	lease, err = store.Claim(ctx, "acct-B")
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, "b-task", lease.TaskID)
}

func TestSemanticDelayUntil(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	// Enqueue two tasks: one immediately visible, one delayed 50ms.
	require.NoError(t, store.Enqueue(ctx, &Task{ID: "now", Type: "t", Priority: PriorityDefault}, WithAccount("a")))
	delay := time.Now().Add(50 * time.Millisecond)
	require.NoError(t, store.Enqueue(ctx, &Task{ID: "later", Type: "t", Priority: PriorityCritical}, WithAccount("a"), WithDelayUntil(delay)))

	// Immediate claim gets "now" even though "later" has higher priority.
	lease, err := store.Claim(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, "now", lease.TaskID)
	require.NoError(t, store.Complete(ctx, lease.ID))

	// "later" is still invisible.
	lease, err = store.Claim(ctx, "a")
	require.NoError(t, err)
	assert.Nil(t, lease, "delayed task should not be visible yet")

	// After the delay elapses, "later" becomes claimable.
	require.Eventually(t, func() bool {
		lease, err = store.Claim(ctx, "a")
		return err == nil && lease != nil && lease.TaskID == "later"
	}, 500*time.Millisecond, 5*time.Millisecond, "delayed task should become visible after delay_until")
	if lease != nil {
		require.NoError(t, store.Complete(ctx, lease.ID))
	}
}

func TestSemanticLeaseRenew(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "r1", Type: "t"}, WithAccount("a")))
	lease, err := store.Claim(ctx, "a", WithLeaseTTL(100*time.Millisecond))
	require.NoError(t, err)
	require.NotNil(t, lease)
	originalExpiry := lease.ExpiresAt

	// Renew for 500ms.
	newExpiry, err := store.Renew(ctx, lease.ID, 500*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, newExpiry.After(originalExpiry), "renew should extend the expiry")

	// Wait past the original TTL — without renew, the task would be stale.
	time.Sleep(120 * time.Millisecond)

	// RecoverStale should NOT re-queue because the lease was renewed.
	n, err := store.RecoverStale(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "renewed lease should not be stale")

	// Complete should still succeed (the lease is still valid).
	require.NoError(t, store.Complete(ctx, lease.ID))
}

func TestSemanticLeaseRecoverStale(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "s1", Type: "t", MaxRetries: 3}, WithAccount("a")))
	lease, err := store.Claim(ctx, "a", WithLeaseTTL(50*time.Millisecond))
	require.NoError(t, err)
	require.NotNil(t, lease)

	// Wait past the lease TTL.
	time.Sleep(80 * time.Millisecond)

	// RecoverStale should re-queue the task.
	n, err := store.RecoverStale(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// The old lease is gone — Complete fails with ErrLeaseNotFound.
	err = store.Complete(ctx, lease.ID)
	assert.ErrorIs(t, err, ErrLeaseNotFound)

	// The task is claimable again.
	lease2, err := store.Claim(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, lease2)
	assert.Equal(t, "s1", lease2.TaskID)
	require.NoError(t, store.Complete(ctx, lease2.ID))
}

func TestSemanticCompleteRemovesTask(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "c1", Type: "t"}, WithAccount("a")))
	lease, err := store.Claim(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, lease)

	require.NoError(t, store.Complete(ctx, lease.ID))

	// No tasks left.
	records, err := store.List(ctx, "a")
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestSemanticAbandonRequeuesWithBackoff(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "ab1", Type: "t", MaxRetries: 3}, WithAccount("a")))
	lease, err := store.Claim(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, 0, lease.Task.Attempts)

	// Abandon with a failure -> requeued (attempts remaining).
	require.NoError(t, store.Abandon(ctx, lease.ID, errors.New("transient")))

	// Task should be pending again but with attempts=1 and a delay_until
	// in the future (backoff). Peek respects delay_until, so it returns nil
	// immediately.
	peeked, err := store.Peek(ctx, "a")
	require.NoError(t, err)
	assert.Nil(t, peeked, "requeued task should be hidden during backoff")

	// List shows the task regardless of delay.
	records, err := store.List(ctx, "a")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "pending", records[0].Status)
	assert.Equal(t, 1, records[0].Attempts)
	assert.Contains(t, records[0].LastError, "transient")

	// After backoff elapses, the task becomes claimable again.
	require.Eventually(t, func() bool {
		lease, err = store.Claim(ctx, "a")
		return err == nil && lease != nil
	}, 500*time.Millisecond, 5*time.Millisecond, "requeued task should become visible after backoff")
	if lease != nil {
		assert.Equal(t, 1, lease.Task.Attempts, "attempt count should persist across requeue")
		assert.Contains(t, lease.Task.LastError, "transient")
		require.NoError(t, store.Complete(ctx, lease.ID))
	}
}

func TestSemanticAbandonDeadLettersAfterExhaustion(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "dl1", Type: "t", MaxRetries: 1}, WithAccount("a")))

	// First attempt: MaxRetries=1, attempts=0 -> 1 <= 1, requeued.
	lease, err := store.Claim(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.NoError(t, store.Abandon(ctx, lease.ID, errors.New("fail-1")))

	// Wait for backoff, then second attempt: attempts=1, 2 > 1 -> DLQ.
	require.Eventually(t, func() bool {
		lease, err = store.Claim(ctx, "a")
		return err == nil && lease != nil
	}, 500*time.Millisecond, 5*time.Millisecond, "requeued task should become visible")
	if lease == nil {
		t.Fatal("lease was nil after backoff")
	}
	require.Equal(t, 1, lease.Task.Attempts)
	require.NoError(t, store.Abandon(ctx, lease.ID, errors.New("fail-2")))

	// Task should now be in the dead-letter queue, not the task table.
	records, err := store.List(ctx, "a")
	require.NoError(t, err)
	assert.Empty(t, records, "task should not be in the active table")

	dl, err := store.DeadLetters(ctx, "a")
	require.NoError(t, err)
	require.Len(t, dl, 1)
	assert.Equal(t, "dl1", dl[0].ID)
	assert.Equal(t, 2, dl[0].Attempts)
	assert.Contains(t, dl[0].LastError, "fail-2")
	assert.False(t, dl[0].DeadAt.IsZero())
}

func TestSemanticAbandonZeroRetriesDeadLettersImmediately(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "z1", Type: "t", MaxRetries: 0}, WithAccount("a")))
	lease, err := store.Claim(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, lease)

	require.NoError(t, store.Abandon(ctx, lease.ID, errors.New("boom")))

	dl, err := store.DeadLetters(ctx, "a")
	require.NoError(t, err)
	require.Len(t, dl, 1)
	assert.Equal(t, "z1", dl[0].ID)
	assert.Equal(t, 1, dl[0].Attempts)
	assert.Equal(t, "boom", dl[0].LastError)
}

func TestSemanticAbandonUnknownLease(t *testing.T) {
	store := newSemanticStore(t)
	err := store.Abandon(context.Background(), "nonexistent-lease", errors.New("x"))
	assert.ErrorIs(t, err, ErrLeaseNotFound)
}

func TestSemanticRenewUnknownLease(t *testing.T) {
	store := newSemanticStore(t)
	_, err := store.Renew(context.Background(), "nonexistent-lease", time.Second)
	assert.ErrorIs(t, err, ErrLeaseNotFound)
}

func TestSemanticCompleteUnknownLease(t *testing.T) {
	store := newSemanticStore(t)
	err := store.Complete(context.Background(), "nonexistent-lease")
	assert.ErrorIs(t, err, ErrLeaseNotFound)
}

func TestSemanticPeek(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "p1", Type: "t", Priority: PriorityLow}, WithAccount("a")))
	require.NoError(t, store.Enqueue(ctx, &Task{ID: "p2", Type: "t", Priority: PriorityCritical}, WithAccount("a")))

	// Peek returns the highest-priority task without claiming it.
	peeked, err := store.Peek(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, peeked)
	assert.Equal(t, "p2", peeked.ID)

	// Peek again — same task, since peek does not claim.
	peeked, err = store.Peek(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, peeked)
	assert.Equal(t, "p2", peeked.ID)

	// Claim also returns p2 (peek did not consume it).
	lease, err := store.Claim(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, "p2", lease.TaskID)
}

func TestSemanticList(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "l1", Type: "t", Priority: PriorityDefault}, WithAccount("a")))
	require.NoError(t, store.Enqueue(ctx, &Task{ID: "l2", Type: "t", Priority: PriorityCritical}, WithAccount("a")))
	require.NoError(t, store.Enqueue(ctx, &Task{ID: "l3", Type: "t", Priority: PriorityLow}, WithAccount("a")))

	// Claim one so we also exercise the 'leased' status.
	lease, err := store.Claim(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, lease)

	records, err := store.List(ctx, "a")
	require.NoError(t, err)
	require.Len(t, records, 3)
	// Order: priority DESC (critical, default, low).
	assert.Equal(t, "l2", records[0].ID)
	assert.Equal(t, PriorityCritical, records[0].Priority)
	assert.Equal(t, "leased", records[0].Status)
	assert.Equal(t, lease.ID, records[0].LeaseID)
	assert.False(t, records[0].LeaseExpiresAt.IsZero())

	assert.Equal(t, "l1", records[1].ID)
	assert.Equal(t, "pending", records[1].Status)

	assert.Equal(t, "l3", records[2].ID)
	assert.Equal(t, "pending", records[2].Status)

	// List across all accounts.
	records, err = store.List(ctx, "")
	require.NoError(t, err)
	assert.Len(t, records, 3)
}

func TestSemanticEnqueueIdempotent(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	task := &Task{ID: "dup", Type: "t", Priority: PriorityDefault}
	require.NoError(t, store.Enqueue(ctx, task, WithAccount("a")))
	// Re-enqueue same ID — ON CONFLICT DO NOTHING, no error.
	require.NoError(t, store.Enqueue(ctx, task, WithAccount("a")))

	records, err := store.List(ctx, "a")
	require.NoError(t, err)
	assert.Len(t, records, 1, "duplicate enqueue should not insert a second row")
}

func TestSemanticEnqueueValidation(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()
	assert.Error(t, store.Enqueue(ctx, nil))
	assert.Error(t, store.Enqueue(ctx, &Task{Type: "t"}))                 // empty ID
	assert.Error(t, store.Enqueue(ctx, &Task{ID: "x"}))                   // empty type
}

func TestSemanticDeleteDeadLetter(t *testing.T) {
	store := newSemanticStore(t)
	ctx := context.Background()

	require.NoError(t, store.Enqueue(ctx, &Task{ID: "d1", Type: "t", MaxRetries: 0}, WithAccount("a")))
	lease, err := store.Claim(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.NoError(t, store.Abandon(ctx, lease.ID, errors.New("perm")))

	dl, err := store.DeadLetters(ctx, "a")
	require.NoError(t, err)
	require.Len(t, dl, 1)

	require.NoError(t, store.DeleteDeadLetter(ctx, "d1"))
	dl, err = store.DeadLetters(ctx, "a")
	require.NoError(t, err)
	assert.Empty(t, dl)
}

func TestSemanticConcurrentClaim(t *testing.T) {
	// Two workers racing on the same queue must each get distinct tasks;
	// no task is claimed twice.
	store := newSemanticStore(t)
	ctx := context.Background()

	const nTasks = 20
	for i := 0; i < nTasks; i++ {
		require.NoError(t, store.Enqueue(ctx, &Task{
			ID:       fmt.Sprintf("c%d", i),
			Type:     "t",
			Priority: PriorityDefault,
		}, WithAccount("race")))
	}

	var claimed int64
	var doubled int64
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				lease, err := store.Claim(ctx, "race")
				if err != nil {
					return
				}
				if lease == nil {
					return
				}
				n := atomic.AddInt64(&claimed, 1)
				if n > nTasks {
					atomic.AddInt64(&doubled, 1)
				}
				if err := store.Complete(ctx, lease.ID); err != nil {
					// Lease was already recovered / completed — a sign of a
					// double-claim bug.
					atomic.AddInt64(&doubled, 1)
				}
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(nTasks), claimed, "every task should be claimed exactly once")
	assert.Equal(t, int64(0), doubled, "no task should be claimed twice")
}
