// Package dedup provides persistent at-least-once event deduplication.
//
// Chat platforms redeliver events (Feishu WS re-push tiers 15s/5min/1h/6h;
// webhook retries; poll overlaps). Inbound handlers must claim the
// message id durably BEFORE acknowledging, or the same message will be
// answered twice.
package dedup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type entry struct {
	SeenAt time.Time `json:"at"`
}

// Store is a bounded, file-backed seen-id set with TTL eviction.
type Store struct {
	mu       sync.Mutex
	path     string
	entries  map[string]entry
	ttl      time.Duration
	maxItems int
	dirty    bool
}

func New(path string, ttl time.Duration, maxItems int) *Store {
	if maxItems <= 0 {
		maxItems = 5000
	}
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	s := &Store{path: path, entries: map[string]entry{}, ttl: ttl, maxItems: maxItems}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.entries)
		s.evictLocked(time.Now())
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	return s
}

// Claim returns true exactly once per id inside the TTL window.
func (s *Store) Claim(id string) bool {
	if id == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if e, ok := s.entries[id]; ok && now.Sub(e.SeenAt) < s.ttl {
		return false
	}
	s.entries[id] = entry{SeenAt: now}
	s.evictLocked(now)
	s.dirty = true
	s.flushLocked()
	return true
}

func (s *Store) evictLocked(now time.Time) {
	if len(s.entries) <= s.maxItems {
		for k, e := range s.entries {
			if now.Sub(e.SeenAt) >= s.ttl {
				delete(s.entries, k)
				s.dirty = true
			}
		}
		return
	}
	// over cap: drop oldest until within budget
	for len(s.entries) > s.maxItems {
		oldestKey := ""
		var oldest time.Time
		first := true
		for k, e := range s.entries {
			if first || e.SeenAt.Before(oldest) {
				oldestKey, oldest = k, e.SeenAt
				first = false
			}
		}
		delete(s.entries, oldestKey)
		s.dirty = true
	}
}

func (s *Store) flushLocked() {
	if !s.dirty {
		return
	}
	b, err := json.Marshal(s.entries)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
	s.dirty = false
}
