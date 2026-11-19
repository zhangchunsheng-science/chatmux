// Package ask implements botmux-style blocking questions: a CLI agent
// (or operator) asks the humans in a chat and blocks until someone
// answers via form card or plain reply, or the timeout expires.
package ask

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

// Pending is one outstanding question bound to a chat.
type Pending struct {
	id      string
	channel string
	chatID  string
	options []string // empty = free-text answer
	ch      chan string
}

// Broker tracks outstanding asks per process.
type Broker struct {
	mu      sync.Mutex
	pending map[string]*Pending // by id
	byChat  map[string]*Pending // "channel|chat" → pending (one per chat)
	seq     int
}

func New() *Broker {
	return &Broker{pending: map[string]*Pending{}, byChat: map[string]*Pending{}}
}

// Register opens a pending question for a chat (one at a time per chat;
// a second Register supersedes and abandons the first).
func (b *Broker) Register(channelName, chatID string, options []string) *Pending {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	p := &Pending{
		id:      fmt.Sprintf("ask-%d-%d", time.Now().UnixNano(), b.seq),
		channel: channelName,
		chatID:  chatID,
		options: options,
		ch:      make(chan string, 1),
	}
	if old, ok := b.byChat[channelName+"|"+chatID]; ok {
		delete(b.pending, old.id)
		// leave old.ch empty: its Wait returns on timeout/cancel
	}
	b.pending[p.id] = p
	b.byChat[channelName+"|"+chatID] = p
	return p
}

// ID is the routing token: form cards carry it as "chatmux_ask:<id>".
func (p *Pending) ID() string { return p.id }

// Wait blocks until resolved, ctx done, or timeout (0 = 10 min default).
func (p *Pending) Wait(ctx context.Context, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case ans := <-p.ch:
		return ans, nil
	case <-tctx.Done():
		return "", tctx.Err()
	}
}

// resolve delivers an answer exactly once.
func (p *Pending) resolve(answer string) bool {
	select {
	case p.ch <- answer:
		return true
	default:
		return false
	}
}

// ResolveForm feeds a form submission whose form name is
// "chatmux_ask:<id>". Returns false when the id is unknown/stale.
func (b *Broker) ResolveForm(form string, values map[string]string) bool {
	id := strings.TrimPrefix(form, "chatmux_ask:")
	if id == form {
		return false
	}
	b.mu.Lock()
	p, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
		delete(b.byChat, p.channel+"|"+p.chatID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	ans := values["answer"]
	for k, v := range values { // tolerate renamed field
		if k != "answer" && v != "" && ans == "" {
			ans = v
		}
	}
	return p.resolve(ans)
}

// TryAnswer consumes an inbound chat message as the answer to a
// pending question in that chat. Option-bound asks accept only listed
// options (free text then flows on to the agent untouched).
func (b *Broker) TryAnswer(msg channel.Message) bool {
	b.mu.Lock()
	p, ok := b.byChat[msg.Channel+"|"+msg.ChatID]
	if ok {
		delete(b.pending, p.id)
		delete(b.byChat, msg.Channel+"|"+msg.ChatID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		// put it back; attachments can't answer for now
		b.mu.Lock()
		b.pending[p.id] = p
		b.byChat[msg.Channel+"|"+msg.ChatID] = p
		b.mu.Unlock()
		return false
	}
	if len(p.options) > 0 {
		for _, o := range p.options {
			if strings.EqualFold(text, o) {
				return p.resolve(o)
			}
		}
		// not a valid option: restore and let the message through
		b.mu.Lock()
		b.pending[p.id] = p
		b.byChat[msg.Channel+"|"+msg.ChatID] = p
		b.mu.Unlock()
		return false
	}
	return p.resolve(text)
}
