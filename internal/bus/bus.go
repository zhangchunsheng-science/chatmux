// Package bus is the in-process message bus connecting channels to agents.
package bus

import (
	"sync"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/metrics"
)

// Bus fan-outs inbound channel messages to subscribers (the router).
type Bus struct {
	mu       sync.Mutex
	subs     []chan channel.Message
	in       chan channel.Message
	inClosed bool // Close ran; the pump may still be draining
	closed   bool // pump finished: drained + subscribers closed
}

func New() *Bus {
	b := &Bus{in: make(chan channel.Message, 128)}
	go func() {
		// Drain semantics: Close closes the inbound side; the pump
		// keeps delivering what channels already pushed, then closes
		// every subscriber so their read loops wind down cleanly.
		for msg := range b.in {
			b.Publish(msg)
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, ch := range b.subs {
			close(ch)
		}
		b.closed = true
	}()
	return b
}

// Inbound is the write side every channel adapter pushes messages to.
// Sending after Close panics (closed channel) — channels are stopped
// before the bus in shutdown order.
func (b *Bus) Inbound() chan<- channel.Message { return b.in }

// Subscribe returns a channel receiving every inbound message. A
// subscriber attached after Close gets a channel that closes once the
// drain finishes (it may still observe the buffered tail).
func (b *Bus) Subscribe() chan channel.Message {
	ch := make(chan channel.Message, 64)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch
	}
	b.subs = append(b.subs, ch)
	return ch
}

// Publish delivers msg to all subscribers, dropping (and counting) for
// slow ones rather than blocking a channel read loop.
func (b *Bus) Publish(msg channel.Message) {
	b.mu.Lock()
	subs := make([]chan channel.Message, len(b.subs))
	copy(subs, b.subs)
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- msg:
		default:
			metrics.BusDrop()
		}
	}
}

// Close stops the bus: the inbound channel closes, buffered messages
// are still delivered, then subscriber channels close. Idempotent.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inClosed {
		return
	}
	b.inClosed = true
	close(b.in)
}
