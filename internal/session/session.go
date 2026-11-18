// Package session keeps per-chat conversation state: history, and the
// turn currently in flight.
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

// Session is the mutable state of one conversation.
type Session struct {
	Key Key

	// History of completed turns (user+assistant), oldest first.
	History []provider.Msg

	// System prompt override for this chat (set via /system command).
	System string
}

// Store is a concurrency-safe session map.
type Store struct {
	mu   sync.Mutex
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

// Append records a completed exchange.
func (s *Store) Append(k Key, user, assistant provider.Msg) {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.History = append(sess.History, user, assistant)
}

// Reset clears history for a chat.
func (s *Store) Reset(k Key) {
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.History = nil
}
