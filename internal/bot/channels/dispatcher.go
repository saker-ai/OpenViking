package channels

import (
	"context"
	"fmt"
	"sync"
)

// Dispatcher routes OutgoingMessages to the Channel that originated
// the conversation. It is the bot's write-side multiplexer: the agent
// loop calls Dispatcher.Send with a channel name and message, and the
// dispatcher forwards to the right adapter.
type Dispatcher struct {
	mu       sync.RWMutex
	channels map[string]Channel
}

// NewDispatcher returns an empty Dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{channels: make(map[string]Channel)}
}

// Register binds a Channel under its Name(). Re-registering the same
// name replaces the prior channel.
func (d *Dispatcher) Register(c Channel) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.channels[c.Name()] = c
}

// Get returns the Channel registered under name, or false.
func (d *Dispatcher) Get(name string) (Channel, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.channels[name]
	return c, ok
}

// Names returns the registered channel names in arbitrary order.
func (d *Dispatcher) Names() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.channels))
	for name := range d.channels {
		out = append(out, name)
	}
	return out
}

// Send forwards msg to the named channel. Returns an error when no
// channel is registered under name.
func (d *Dispatcher) Send(ctx context.Context, msg OutgoingMessage, channelName string) error {
	d.mu.RLock()
	c, ok := d.channels[channelName]
	d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("channels: unknown channel %q", channelName)
	}
	return c.Send(ctx, msg)
}

// StartAll launches every registered channel's Start loop concurrently
// and returns when ctx is canceled or any channel returns an error.
// The first error is returned; subsequent errors are logged.
func (d *Dispatcher) StartAll(ctx context.Context, handler Handler) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.channels) == 0 {
		return fmt.Errorf("channels: no channels registered")
	}
	errCh := make(chan error, len(d.channels))
	for name, c := range d.channels {
		go func(name string, c Channel) {
			err := c.Start(ctx, func(ctx context.Context, m IncomingMessage) error {
				// Stamp the channel name onto the message so the agent
				// loop knows where to send replies.
				m.ChannelName = name
				return handler(ctx, m)
			})
			if err != nil {
				errCh <- fmt.Errorf("channel %s: %w", name, err)
			}
		}(name, c)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}
