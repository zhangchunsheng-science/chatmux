// Package bus is the in-process message bus connecting channels to agents.
package bus

import (
	"sync"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

// Bus fan-outs inbound channel messages to subscribers (the router).
type Bus struct {
	mu   sync.Mutex
	subs []chan channel.Message
	in   chan channel.Message
}

func New() *Bus {
	b := &Bus{in: make(chan channel.Message, 128)}
	go func() {
		for msg := range b.in {
			b.Publish(msg)
		}
	}()
	return b
}

// Inbound is the write side every channel adapter pushes messages to.
func (b *Bus) Inbound() chan<- channel.Message { return b.in }

// Subscribe returns a channel receiving every inbound message.
func (b *Bus) Subscribe() chan channel.Message {
	ch := make(chan channel.Message, 64)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return ch
}

// Publish delivers msg to all subscribers, dropping for slow ones
// rather than blocking a channel read loop.
func (b *Bus) Publish(msg channel.Message) {
	b.mu.Lock()
	subs := make([]chan channel.Message, len(b.subs))
	copy(subs, b.subs)
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- msg:
		default:
		}
	}
}
