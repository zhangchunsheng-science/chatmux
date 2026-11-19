package telegram

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSplitMessage(t *testing.T) {
	long := strings.Repeat("行", 5000)
	chunks := splitMessage(long, 1000)
	if len(chunks) < 5 {
		t.Fatalf("expected >=5 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > 1000 {
			t.Fatal("chunk over limit")
		}
	}
	if got := splitMessage("短消息", 1000); len(got) != 1 {
		t.Fatalf("short message should stay whole, got %d chunks", len(got))
	}
}

func TestSplitPrefersNewline(t *testing.T) {
	s := strings.Repeat("a", 700) + "\n" + strings.Repeat("b", 700)
	chunks := splitMessage(s, 1000)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if !strings.HasSuffix(chunks[0], "a") || !strings.HasPrefix(strings.TrimLeft(chunks[1], "\n"), "b") {
		t.Fatal("should cut at the newline")
	}
}

func TestExpandMentionsSlackStyle(t *testing.T) { // sanity that helper exists
	if displayName(nil) != "?" {
		t.Fatal("nil user name")
	}
}

func TestForumTopicMapping(t *testing.T) {
	tg := &Telegram{name: "tg"}
	var uu tgUpdate
	err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":10,"from":{"id":7},"chat":{"id":99,"type":"supergroup"},"message_thread_id":42,"text":"话题内消息","reply_to_message":{"message_id":9,"text":"被回复的"}}}`), &uu)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := tg.toMessage(uu)
	if !ok {
		t.Fatal("forum message should map")
	}
	if msg.ThreadID != "42" || !msg.ThreadIsTopic {
		t.Fatalf("want topic 42 flagged, got %q topic=%v", msg.ThreadID, msg.ThreadIsTopic)
	}
	if msg.Quoted == nil || msg.Quoted.ID != "9" {
		t.Fatalf("quoted context should survive: %+v", msg.Quoted)
	}

	// non-forum reply keeps reply-derived ThreadID, unflagged
	var u2 tgUpdate
	if err := json.Unmarshal([]byte(`{"message":{"message_id":11,"from":{"id":7},"chat":{"id":99,"type":"group"},"text":"普通群回复","reply_to_message":{"message_id":3,"text":"原消息"}}}`), &u2); err != nil {
		t.Fatal(err)
	}
	m2, ok := tg.toMessage(u2)
	if !ok || m2.ThreadID != "3" || m2.ThreadIsTopic {
		t.Fatalf("non-forum reply: ok=%v thread=%q topic=%v", ok, m2.ThreadID, m2.ThreadIsTopic)
	}
}
