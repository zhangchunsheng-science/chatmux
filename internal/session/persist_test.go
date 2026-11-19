package session

import (
	"os"
	"path/filepath"
	"testing"

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
