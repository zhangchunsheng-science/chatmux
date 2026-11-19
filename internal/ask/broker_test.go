package ask

import (
	"context"
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

func TestAskFormRoundTrip(t *testing.T) {
	b := New()
	p := b.Register("feishu", "oc_1", nil)
	if p.ID() == "" {
		t.Fatal("no ask id")
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		if !b.ResolveForm("chatmux_ask:"+p.ID(), map[string]string{"answer": "用方案B"}) {
			t.Error("ResolveForm should succeed")
		}
	}()
	ans, err := p.Wait(context.Background(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ans != "用方案B" {
		t.Fatalf("answer = %q", ans)
	}
	// stale resolve after completion is rejected
	if b.ResolveForm("chatmux_ask:"+p.ID(), map[string]string{"answer": "late"}) {
		t.Error("stale resolve should fail")
	}
}

func TestAskTextFallbackOptions(t *testing.T) {
	b := New()
	p := b.Register("telegram", "42", []string{"A", "B"})
	msg := channel.Message{Channel: "telegram", ChatID: "42", Text: "随便说"}
	if b.TryAnswer(msg) {
		t.Fatal("non-option text must not be consumed")
	}
	ok := b.TryAnswer(channel.Message{Channel: "telegram", ChatID: "42", Text: "b"})
	if !ok {
		t.Fatal("option match (case-insensitive) should consume")
	}
	ans, err := p.Wait(context.Background(), time.Second)
	if err != nil || ans != "B" {
		t.Fatalf("ans=%q err=%v", ans, err)
	}
	// after resolution the chat is free again
	if b.TryAnswer(channel.Message{Channel: "telegram", ChatID: "42", Text: "A"}) {
		t.Fatal("no pending ask should remain")
	}
}

func TestAskTimeout(t *testing.T) {
	b := New()
	p := b.Register("feishu", "oc_2", nil)
	if _, err := p.Wait(context.Background(), 50*time.Millisecond); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestAskSupersede(t *testing.T) {
	b := New()
	p1 := b.Register("feishu", "oc_3", nil)
	b.Register("feishu", "oc_3", nil) // supersedes
	if b.ResolveForm("chatmux_ask:"+p1.ID(), map[string]string{"answer": "x"}) {
		t.Fatal("superseded ask must be dead")
	}
}
