package bus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/channels"
	"github.com/saker-ai/ctxhub/internal/queuefs"
)

func TestBus_PublishSubscribe(t *testing.T) {
	b := New(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub := b.Subscribe(ctx)
	msg := channels.IncomingMessage{ChatID: "1", Text: "hi", ChannelName: "test"}
	if err := b.Publish(ctx, msg); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case got := <-sub:
		if got.ChatID != "1" || got.Text != "hi" {
			t.Errorf("got = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("Subscribe did not receive")
	}
}

func TestBus_PublishCanceledContext(t *testing.T) {
	b := New(1)
	ctx, cancel := context.WithCancel(context.Background())
	// Fill the queue so the send cannot succeed; ctx.Done() must win.
	if err := b.Publish(context.Background(), channels.IncomingMessage{}); err != nil {
		t.Fatalf("seed Publish: %v", err)
	}
	cancel()
	err := b.Publish(ctx, channels.IncomingMessage{})
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestBus_ShutdownDrainsSubscribers(t *testing.T) {
	b := New(8)
	ctx, cancel := context.WithCancel(context.Background())
	sub := b.Subscribe(ctx)
	// Cancel ctx so the subscriber exits, then Shutdown should return.
	cancel()
	shutdownCtx, scancel := context.WithTimeout(context.Background(), time.Second)
	defer scancel()
	if err := b.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Subscriber channel should be closed.
	if _, ok := <-sub; ok {
		t.Errorf("sub should be closed after Shutdown")
	}
}

// fakeQueue is a minimal queuefs.QueueServer for the bus tests. It
// counts Enqueue calls so we can assert the durable path was used.
type fakeQueue struct {
	enqueued atomic.Int32
	handlers map[string]queuefs.HandlerFunc
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{handlers: make(map[string]queuefs.HandlerFunc)}
}

func (f *fakeQueue) Start(ctx context.Context) error { return nil }
func (f *fakeQueue) Shutdown() error                 { return nil }
func (f *fakeQueue) Enqueue(task *queuefs.Task) error {
	f.enqueued.Add(1)
	return nil
}
func (f *fakeQueue) RegisterHandler(taskType string, handler queuefs.HandlerFunc) {
	f.handlers[taskType] = handler
}

func TestBus_WithQueuefs_EnqueuesDurable(t *testing.T) {
	b := New(8)
	q := newFakeQueue()
	b.WithQueuefs(q)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Publish(ctx, channels.IncomingMessage{ChatID: "c1", UserID: "u1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if q.enqueued.Load() != 1 {
		t.Errorf("enqueued = %d, want 1", q.enqueued.Load())
	}
	if _, ok := q.handlers["vikingbot.message"]; !ok {
		t.Errorf("handler not registered")
	}
}
