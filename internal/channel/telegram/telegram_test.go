package telegram

import (
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
