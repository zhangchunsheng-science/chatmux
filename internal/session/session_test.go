package session

import (
	"strings"
	"testing"

	"github.com/zhangchunsheng-science/chatmux/internal/provider"
)

func TestAppendCapsMessageSize(t *testing.T) {
	// a pathological paste must not be stored in full: the whole
	// sessions.json re-marshals on every save
	s := NewStore()
	k := Key{Channel: "feishu", ChatID: "c1"}
	big := strings.Repeat("头", 300<<10) + "尾部标记" // ~900KB UTF-8
	s.Append(k, provider.Msg{Role: provider.User, Content: big},
		provider.Msg{Role: provider.Assistant, Content: "收到"})
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	got := sess.History[0].Content
	if len(got) > maxStoredMessageBytes+128 {
		t.Fatalf("stored %d bytes, want ≤ cap+marker", len(got))
	}
	if !strings.Contains(got, "已截断") {
		t.Fatal("truncation marker missing")
	}
	if !strings.HasSuffix(got, "尾部标记") {
		t.Fatal("tail must be preserved")
	}
}

func TestAppendSessionByteBudget(t *testing.T) {
	// many medium messages (each under the per-message cap) must still
	// be bounded by the session byte budget, oldest pairs dropped first
	s := NewStore()
	k := Key{Channel: "feishu", ChatID: "c1"}
	body := strings.Repeat("x", 60<<10) // 60KB, under the 64KB message cap
	for i := 0; i < 20; i++ {           // 20 turns × ~120KB = 2.4MB raw
		s.Append(k,
			provider.Msg{Role: provider.User, Content: body + "-最新" + string(rune('a'+i))},
			provider.Msg{Role: provider.Assistant, Content: body})
	}
	sess := s.Get(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := sessionBytes(sess.History); n > maxSessionBytes {
		t.Fatalf("session stored %d bytes, over the %d budget", n, maxSessionBytes)
	}
	last := sess.History[len(sess.History)-2].Content // latest user msg
	if !strings.HasSuffix(last, "-最新t") {
		t.Fatalf("latest pair must be kept, got %q…", last[:50])
	}
}
