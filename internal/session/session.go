// Package session keeps per-chat conversation state: history, model
// override, and the turn currently in flight.
package session

import (
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/provider"
)

// Key identifies a conversation anchor: channel + chat + optional thread.
// Thread-anchored chats (topic groups, reply threads) get one session
// per thread root, mirroring botmux's anchor routing.
type Key struct {
	Channel  string
	ChatID   string
	ThreadID string
}

// String is the anchor id used to key CLI-agent resume state too.
func (k Key) String() string {
	if k.ThreadID != "" {
		return k.Channel + "|" + k.ChatID + "|" + k.ThreadID
	}
	return k.Channel + "|" + k.ChatID
}

// Session is the mutable state of one conversation.
type Session struct {
	Key Key

	// History of completed turns (user+assistant), oldest first.
	History []provider.Msg

	// System prompt override for this chat (set via /system).
	System string

	// Model override for this chat (set via /model).
	Model string

	// Title names this conversation for /status and the dashboard
	// (set via /name; chatmux's counterpart to session rename).
	Title string

	// Agent overrides route-based agent selection for this chat
	// (set via /agent; botmux handoff's counterpart).
	Agent string

	// LastUsed stamps the most recent Append/Set; persistence evicts
	// the stalest sessions past maxPersistedSessions.
	LastUsed time.Time
}

// maxHistory bounds stored turns per chat (user+assistant pairs).
const maxHistory = 40

// Byte caps: count alone doesn't bound the store — one pasted dump can
// be megabytes, and the whole sessions.json re-marshals on every save.
// maxStoredMessageBytes caps a single message (head+tail preserved);
// maxSessionBytes caps one session's history at ~5× the replay
// character budget, so anything replay could never use is dropped
// before it becomes dead weight in memory and on disk.
const (
	maxStoredMessageBytes = 64 << 10
	maxSessionBytes       = 400 << 10
)

// capMessage truncates an over-long message body, keeping head and
// tail (the tail is where replies and code land).
func capMessage(m provider.Msg) provider.Msg {
	if len(m.Content) <= maxStoredMessageBytes {
		return m
	}
	const keep = 16 << 10
	head := maxStoredMessageBytes - keep
	m.Content = m.Content[:head] + "\n…[消息超长，已截断]…\n" + m.Content[len(m.Content)-keep:]
	return m
}

// sessionBytes is the total stored body size of a history slice.
func sessionBytes(h []provider.Msg) int {
	n := 0
	for _, m := range h {
		n += len(m.Content)
	}
	return n
}

// Store is a concurrency-safe session map.
type Store struct {
	mu    sync.Mutex
	byKey map[Key]*Session
}

func NewStore() *Store {
	return &Store{byKey: map[Key]*Session{}}
}

func (s *Store) Get(k Key) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byKey[k]
	if !ok {
		sess = &Session{Key: k}
		s.byKey[k] = sess
	}
	return sess
}

// Set updates mutable overrides (system/model) for a chat.
func (s *Store) Set(k Key, fn func(*Session)) {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(sess)
	sess.LastUsed = time.Now()
}

// Append records a completed exchange, trimming to maxHistory messages.
func (s *Store) Append(k Key, user, assistant provider.Msg) {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.History = append(sess.History, capMessage(user), capMessage(assistant))
	if n := len(sess.History); n > maxHistory {
		sess.History = sess.History[n-maxHistory:]
	}
	// byte budget: drop the oldest pair while over cap, always keeping
	// the latest pair
	for sessionBytes(sess.History) > maxSessionBytes && len(sess.History) > 2 {
		sess.History = sess.History[2:]
	}
	sess.LastUsed = time.Now()
}

// Snapshot returns a copy of a chat's stored history (for /history).
func (s *Store) Snapshot(k Key) []provider.Msg {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]provider.Msg, len(sess.History))
	copy(out, sess.History)
	return out
}

// Count returns the number of stored messages for a chat.
func (s *Store) Count(k Key) int {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(sess.History)
}

// Reset clears everything (history + overrides + title) for a chat.
func (s *Store) Reset(k Key) {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.History = nil
	sess.System = ""
	sess.Model = ""
	sess.Title = ""
	sess.Agent = ""
}

// Rewind drops the last n exchanges (2n messages) from a chat's
// history — /rewind: the model forgets, the chat log stays.
func (s *Store) Rewind(k Key, n int) int {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	drop := n * 2
	if drop > len(sess.History) {
		drop = len(sess.History)
	}
	sess.History = sess.History[:len(sess.History)-drop]
	return drop / 2
}

// Info is a read-only session view for dashboards.
type Info struct {
	Key      Key
	Title    string
	Messages int
	Model    string
	Agent    string
}

// Infos snapshots all sessions for the dashboard.
func (s *Store) Infos() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Info, 0, len(s.byKey))
	for k, sess := range s.byKey {
		out = append(out, Info{Key: k, Title: sess.Title, Messages: len(sess.History), Model: sess.Model, Agent: sess.Agent})
	}
	return out
}

// Len returns the number of live sessions.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byKey)
}
