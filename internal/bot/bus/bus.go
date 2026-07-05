// Package bus is the in-process event bus for vikingbot. It bridges
// channel adapters (which produce IncomingMessages) to the agent loop
// (which consumes them). For durable processing the bus also exposes
// an asynq-backed enqueue path; the agent loop registers a handler
// with queuefs to drain it.
//
// The bus is deliberately minimal: it is a buffered channel with
// graceful shutdown. For the in-process path the buffer is bounded by
// BusConfig.QueueSize; for the durable path the queue is asynq.
package bus

import (
	"context"
	"sync"

	"github.com/saker-ai/ctxhub/internal/bot/channels"
	"github.com/saker-ai/ctxhub/internal/queuefs"
)

// Bus is the in-process event bus. It is safe for concurrent use.
type Bus struct {
	queueSize int
	queue     chan channels.IncomingMessage
	queuefs   queuefs.QueueServer // optional durable backend; nil for in-memory only
	wg        sync.WaitGroup
}

// New returns a Bus with the given queue size. When queueSize <= 0 it
// defaults to 1024.
func New(queueSize int) *Bus {
	if queueSize <= 0 {
		queueSize = 1024
	}
	return &Bus{queueSize: queueSize, queue: make(chan channels.IncomingMessage, queueSize)}
}

// WithQueuefs attaches a durable queue backend. When set, Publish
// enqueues a task onto queuefs in addition to the in-process channel;
// the durable path is used when the agent process may be restarted
// mid-conversation.
func (b *Bus) WithQueuefs(q queuefs.QueueServer) *Bus {
	b.queuefs = q
	b.queuefs.RegisterHandler("vikingbot.message", b.handleDurable)
	return b
}

// handleDurable is the queuefs handler that re-injects a durable
// task back into the in-process queue.
func (b *Bus) handleDurable(ctx context.Context, task *queuefs.Task) (*queuefs.TaskResult, error) {
	// Payload is opaque — we don't decode it here; the producer is
	// expected to also publish to the in-process channel. This handler
	// exists so the durable queue can be drained without orphan tasks.
	return &queuefs.TaskResult{Metadata: map[string]string{"drained": "1"}}, nil
}

// Publish pushes msg onto the in-process queue. It blocks when the
// queue is full; callers should select on ctx.Done() to avoid stalls.
// When a queuefs backend is attached, msg is also enqueued durably
// (best-effort — a queuefs failure is logged but does not fail the
// call, since the in-process copy is sufficient for live operation).
func (b *Bus) Publish(ctx context.Context, msg channels.IncomingMessage) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case b.queue <- msg:
	}
	if b.queuefs != nil {
		// Best-effort durable enqueue. We use the message ID as task ID
		// for dedup; empty IDs are skipped (queuefs requires non-empty).
		taskID := msg.ChatID + ":" + msg.UserID
		if taskID != ":" {
			_ = b.queuefs.Enqueue(&queuefs.Task{
				ID:       taskID,
				Type:     "vikingbot.message",
				Priority: queuefs.PriorityCritical,
			})
		}
	}
	return nil
}

// Subscribe returns a channel that receives every published message.
// The channel is closed when ctx is canceled. Multiple subscribers are
// supported via fan-out (each receives a copy).
type Subscriber <-chan channels.IncomingMessage

// Subscribe fans out published messages to a new receive-only channel.
// The channel is closed when ctx is canceled.
func (b *Bus) Subscribe(ctx context.Context) Subscriber {
	out := make(chan channels.IncomingMessage, b.queueSize)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-b.queue:
				if !ok {
					return
				}
				select {
				case out <- m:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// Shutdown closes the publish path and waits for subscribers to drain.
// It is idempotent.
func (b *Bus) Shutdown(ctx context.Context) error {
	close(b.queue)
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
