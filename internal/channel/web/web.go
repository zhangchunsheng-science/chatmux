// Package web is the dashboard chat console channel: the operator talks
// to the gateway from the admin page — chatmux's take on botmux's
// 网页会话. Inbound lines arrive via HTTP, outbound replies park in a
// per-chat ring buffer the dashboard polls.
package web

import (
	"context"
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

// Entry is one line of the console log (both directions).
type Entry struct {
	At    time.Time `json:"at"`
	From  string    `json:"from"` // "you" | bot name | "system"
	Text  string    `json:"text"`
	Final bool      `json:"final"`
}

// Web is a channel.Channel over an in-memory log.
type Web struct {
	name string

	mu      sync.Mutex
	log     []Entry
	maxLog  int
	inbound chan channel.Message
}

// New builds a web console channel (name default "web").
func New(name string) *Web {
	if name == "" {
		name = "web"
	}
	return &Web{name: name, maxLog: 500, inbound: make(chan channel.Message, 16)}
}

func (w *Web) Name() string { return w.name }

// Start just drains until ctx is done (inbound is pushed via Inject).
func (w *Web) Start(ctx context.Context, out chan<- channel.Message) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-w.inbound:
			select {
			case out <- msg:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Inject queues one console line as an inbound message.
func (w *Web) Inject(chatID, userID, userName, text string) {
	msg := channel.Message{
		ID:      "web-" + time.Now().Format("150405.000000000"),
		Channel: w.name, ChatID: chatID, ChatType: "p2p",
		UserID: userID, UserName: userName, MentionedMe: true,
		Text: text, Timestamp: time.Now(),
	}
	w.append(Entry{At: msg.Timestamp, From: "you", Text: text, Final: true})
	select {
	case w.inbound <- msg:
	default: // console full — drop with a note rather than block HTTP
		w.append(Entry{At: time.Now(), From: "system", Text: "（消息队列满，已丢弃）", Final: true})
	}
}

func (w *Web) append(e Entry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, e)
	if n := len(w.log); n > w.maxLog {
		w.log = w.log[n-w.maxLog:]
	}
}

// Log returns entries newer than the given index (0 = all); the return
// is the new cursor.
func (w *Web) Log(since int) ([]Entry, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if since < 0 || since > len(w.log) {
		since = 0
	}
	out := make([]Entry, len(w.log)-since)
	copy(out, w.log[since:])
	return out, len(w.log)
}

// sender delivers runtime output into the log.
type sender struct{ w *Web }

func (w *Web) SenderFor(msg channel.Message) (channel.Sender, error) {
	return sender{w: w}, nil
}

func (s sender) Send(ctx context.Context, text string, opts channel.SendOptions) (string, error) {
	s.w.append(Entry{At: time.Now(), From: "bot", Text: text, Final: opts.Final})
	return "", nil
}

func (s sender) Replace(ctx context.Context, msgID, text string) error {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	// find last bot entry and update in place (streaming feel)
	for i := len(s.w.log) - 1; i >= 0; i-- {
		if s.w.log[i].From == "bot" && !s.w.log[i].Final {
			s.w.log[i].Text = text
			return nil
		}
	}
	s.w.log = append(s.w.log, Entry{At: time.Now(), From: "bot", Text: text})
	if n := len(s.w.log); n > s.w.maxLog {
		s.w.log = s.w.log[n-s.w.maxLog:]
	}
	return nil
}

func (s sender) React(ctx context.Context, msgID, emoji string) error { return nil }

func (w *Web) Healthy() bool { return true }
