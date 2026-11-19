package web

import (
	"context"
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

func TestConsoleRoundTrip(t *testing.T) {
	w := New("")
	if w.Name() != "web" {
		t.Fatalf("name %q", w.Name())
	}
	out := make(chan channel.Message, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx, out) }()

	w.Inject("console", "web:me", "me", "你好")
	select {
	case msg := <-out:
		if msg.ChatID != "console" || msg.Text != "你好" || !msg.MentionedMe {
			t.Fatalf("msg %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("inbound not delivered")
	}

	entries, cursor := w.Log(0)
	if len(entries) != 1 || entries[0].From != "you" || entries[0].Text != "你好" {
		t.Fatalf("entries %+v", entries)
	}

	// runtime reply flows through SenderFor
	s, err := w.SenderFor(channel.Message{Channel: "web", ChatID: "console"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "正在想…", channel.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace(context.Background(), "", "答案来了"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "最终答案", channel.SendOptions{Final: true}); err != nil {
		t.Fatal(err)
	}

	entries, _ = w.Log(cursor) // incremental from old cursor
	if len(entries) != 2 {
		t.Fatalf("want 2 entries (replace edits in place), got %d: %+v", len(entries), entries)
	}
	// Replace updated the interim entry in place
	if entries[0].Text != "答案来了" {
		t.Fatalf("replace did not update interim: %+v", entries[0])
	}
	if !entries[1].Final {
		t.Fatalf("final flag lost: %+v", entries[1])
	}
}

func TestLogCursorClamp(t *testing.T) {
	w := New("cw")
	w.Inject("c", "u", "n", "hi")
	if entries, c := w.Log(99); len(entries) != 1 || c != 1 {
		t.Fatalf("out-of-range cursor should clamp to 0: %d %+v", c, entries)
	}
}
