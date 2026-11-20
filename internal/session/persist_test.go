package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/provider"
)

func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore()
	k := Key{Channel: "feishu", ChatID: "oc_1", ThreadID: "om_t1"}
	s.Set(k, func(x *Session) {
		x.System = "custom system"
		x.Model = "gpt-4o-mini"
	})
	s.Append(k, provider.Msg{Role: provider.User, Content: "hi"},
		provider.Msg{Role: provider.Assistant, Content: "hello"})
	// background-job pseudo keys must not persist
	jk := Key{Channel: "feishu", ChatID: "oc_1|jobbg-9"}
	s.Append(jk, provider.Msg{Role: provider.User, Content: "bg"}, provider.Msg{Role: provider.Assistant, Content: "done"})

	if err := s.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); err != nil {
		t.Fatalf("sessions.json missing: %v", err)
	}

	got := LoadSessions(dir)
	back := got.Get(k)
	if len(back.History) != 2 || back.History[0].Content != "hi" {
		t.Fatalf("history not restored: %+v", back.History)
	}
	if back.System != "custom system" || back.Model != "gpt-4o-mini" {
		t.Fatalf("overrides not restored: system=%q model=%q", back.System, back.Model)
	}
	// Len first — Get(jk) below would lazily create the empty entry
	if got.Len() != 1 {
		t.Fatalf("want 1 persisted session, got %d", got.Len())
	}
	if len(got.Get(jk).History) != 0 {
		t.Fatalf("job pseudo-session leaked into persistence: %+v", got.Get(jk))
	}
}

func TestParseAnchor(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Key
		ok   bool
	}{
		{"feishu|oc_1", Key{Channel: "feishu", ChatID: "oc_1"}, true},
		{"feishu|oc_1|om_t9", Key{Channel: "feishu", ChatID: "oc_1", ThreadID: "om_t9"}, true},
		{"onlyone", Key{}, false},
		{"|oc_1", Key{}, false},
	} {
		k, ok := parseAnchor(tc.in)
		if ok != tc.ok || (ok && k != tc.want) {
			t.Errorf("parseAnchor(%q) = %+v,%v want %+v,%v", tc.in, k, ok, tc.want, tc.ok)
		}
	}
}

func TestSavePrunesEmptySessions(t *testing.T) {
	dir := t.TempDir()
	s := NewStore()
	// pure Get() artifact: no history, no overrides
	s.Get(Key{Channel: "feishu", ChatID: "ghost"})
	// override-only session survives (it's deliberate state)
	s.Set(Key{Channel: "feishu", ChatID: "kept"}, func(x *Session) { x.Model = "gpt" })
	// lived-in session survives
	s.Append(Key{Channel: "feishu", ChatID: "real"}, provider.Msg{Role: "user", Content: "hi"}, provider.Msg{Role: "assistant", Content: "yo"})
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 {
		t.Fatalf("empty session not pruned from memory: len=%d", s.Len())
	}
	loaded := LoadSessions(dir)
	if loaded.Len() != 2 {
		t.Fatalf("want 2 persisted sessions, got %d", loaded.Len())
	}
	if _, ok := loaded.byKey[Key{Channel: "feishu", ChatID: "ghost"}]; ok {
		t.Fatal("ghost session persisted")
	}
}

func TestSaveCapsPersistedSessions(t *testing.T) {
	dir := t.TempDir()
	s := NewStore()
	for i := 0; i < maxPersistedSessions+1; i++ {
		k := Key{Channel: "feishu", ChatID: fmt.Sprintf("c%04d", i)}
		s.Append(k, provider.Msg{Role: "user", Content: "hi"}, provider.Msg{Role: "assistant", Content: "yo"})
		// manual age: earlier chats used long ago, later chats recently
		s.byKey[k].LastUsed = time.Now().Add(time.Duration(i-maxPersistedSessions) * time.Hour)
	}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded := LoadSessions(dir)
	if loaded.Len() != maxPersistedSessions {
		t.Fatalf("want cap %d, got %d", maxPersistedSessions, loaded.Len())
	}
	if _, ok := loaded.byKey[Key{Channel: "feishu", ChatID: "c0000"}]; ok {
		t.Fatal("stalest session should have been evicted")
	}
	if _, ok := loaded.byKey[Key{Channel: "feishu", ChatID: "c0001"}]; !ok {
		t.Fatal("second-stalest should survive")
	}
}

func TestAppendStampsLastUsed(t *testing.T) {
	s := NewStore()
	k := Key{Channel: "feishu", ChatID: "c1"}
	old := time.Now().Add(-time.Hour)
	s.Append(k, provider.Msg{Role: "user", Content: "hi"}, provider.Msg{Role: "assistant", Content: "yo"})
	s.mu.Lock()
	stamp := s.byKey[k].LastUsed
	s.mu.Unlock()
	if stamp.Before(old.Add(time.Hour)) {
		t.Fatalf("LastUsed not stamped: %v", stamp)
	}
}

// Concurrent Save calls (every turn fires one asynchronously) must
// never corrupt sessions.json: with the old fixed "<path>.tmp", two
// overlapping saves interleaved bytes in one temp file and the rename
// published the mixture — a corrupt file that LoadSessions silently
// swallows by starting empty.
func TestSaveConcurrentDoesNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	s := NewStore()
	for i := 0; i < 5; i++ {
		key := Key{Channel: "web", ChatID: fmt.Sprintf("c%d", i)}
		sess := s.Get(key)
		sess.History = append(sess.History, provider.Msg{Role: "user", Content: "hi"})
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Save(dir); err != nil {
				t.Errorf("save: %v", err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("sessions.json corrupt after concurrent saves: %v", err)
	}
	if len(in) != 5 {
		t.Fatalf("restored %d sessions, want 5", len(in))
	}
}
