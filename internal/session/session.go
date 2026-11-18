// Package session keeps per-chat conversation state: history, model
// override, and the turn currently in flight.
package session

import (
	"sync"

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
}

// maxHistory bounds stored turns per chat (user+assistant pairs).
const maxHistory = 40

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
}

// Append records a completed exchange, trimming to maxHistory messages.
func (s *Store) Append(k Key, user, assistant provider.Msg) {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.History = append(sess.History, user, assistant)
	if n := len(sess.History); n > maxHistory {
		sess.History = sess.History[n-maxHistory:]
	}
}

// Count returns the number of stored messages for a chat.
func (s *Store) Count(k Key) int {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(sess.History)
}

// Reset clears everything (history + overrides) for a chat.
func (s *Store) Reset(k Key) {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.History = nil
	sess.System = ""
	sess.Model = ""
}

// Len returns the number of live sessions.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byKey)
}
