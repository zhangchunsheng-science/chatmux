package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/provider"
	"github.com/zhangchunsheng-science/chatmux/internal/session"
)

// fakeSender records outbound messages.
type fakeSender struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeSender) Send(_ context.Context, text string, _ channel.SendOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return "m" + string(rune('0'+len(f.sent))), nil
}
func (f *fakeSender) Replace(_ context.Context, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeSender) React(_ context.Context, _, _ string) error { return nil }

func (f *fakeSender) joined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.sent, "\n---\n")
}

// waitContains polls for an async reply (commands send via goroutine).
func waitContains(t *testing.T, s *fakeSender, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.joined(), substr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for reply containing %q; got:\n%s", substr, s.joined())
}

func testRuntime() (*Runtime, session.Key) {
	off := false
	on := true
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{Name: "default", Provider: "openai"},
			{Name: "gated", GroupMention: &on, Routes: []config.Route{{ChatID: "g1"}}},
			{Name: "open", GroupMention: &off, Routes: []config.Route{{ChatID: "lounge"}}},
		},
		DefaultAgent: "default",
	}
	rt := NewRuntime(cfg, provider.NewRegistry(), map[string]channel.Channel{})
	key := session.Key{Channel: "feishu", ChatID: "c1"}
	return rt, key
}

func TestMentionGate(t *testing.T) {
	rt, _ := testRuntime()

	blocked := channel.Message{Channel: "feishu", ChatID: "g1", ChatType: "group", MentionedMe: false}
	if mentionAllowed(rt.agentFor(blocked), blocked) {
		t.Errorf("gated agent should block un-mentioned group messages")
	}
	withMention := channel.Message{Channel: "feishu", ChatID: "g1", ChatType: "group", MentionedMe: true}
	if !mentionAllowed(rt.agentFor(withMention), withMention) {
		t.Errorf("mentioned group message should pass")
	}
	p2p := channel.Message{Channel: "feishu", ChatID: "c1", ChatType: "p2p", MentionedMe: false}
	if !mentionAllowed(rt.agentFor(p2p), p2p) {
		t.Errorf("p2p should never need mention")
	}
	noMention := channel.Message{Channel: "feishu", ChatID: "lounge", ChatType: "group", MentionedMe: false}
	a := rt.agentFor(noMention)
	if a == nil || a.Name != "open" {
		t.Fatalf("lounge should route to open agent, got %+v", a)
	}
	if !mentionAllowed(a, noMention) {
		t.Errorf("groupMention=false agent should answer without @")
	}
}

func TestSlashCommands(t *testing.T) {
	rt, key := testRuntime()
	a := &rt.cfg.Agents[0]
	s := &fakeSender{}
	cmd := func(text string) bool {
		return rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: text}, a, s)
	}

	// /help
	if !cmd("/help") {
		t.Fatalf("/help should be a command")
	}
	waitContains(t, s, "/stop")

	// /model switch + query
	if !cmd("/model gpt-4o-mini") {
		t.Fatalf("/model should be a command")
	}
	if got := rt.sessions.Get(key).Model; got != "gpt-4o-mini" {
		t.Errorf("session model = %q", got)
	}
	if !cmd("/model") {
		t.Fatalf("bare /model should be a command")
	}
	waitContains(t, s, "当前模型: gpt-4o-mini")

	// /new resets history + overrides
	rt.sessions.Append(key, provider.Msg{Role: provider.User, Content: "hi"}, provider.Msg{Role: provider.Assistant, Content: "yo"})
	if !cmd("/new") {
		t.Fatalf("/new should be a command")
	}
	if n := rt.sessions.Count(key); n != 0 {
		t.Errorf("/new should clear history, has %d", n)
	}
	if got := rt.sessions.Get(key).Model; got != "" {
		t.Errorf("/new should clear model override, got %q", got)
	}

	// /status with no in-flight turn
	if !cmd("/status") {
		t.Fatalf("/status should be a command")
	}
	waitContains(t, s, "空闲")

	// /stop with nothing running
	if !cmd("/stop") {
		t.Fatalf("/stop should be a command")
	}
	waitContains(t, s, "没有在处理")

	// unknown slash falls through to the model
	if cmd("/verbosity 3") {
		t.Errorf("unknown /command should not be intercepted")
	}
}

func TestStatus(t *testing.T) {
	rt, _ := testRuntime()
	st := rt.Status()
	if st["turnsInFlight"].(int) != 0 {
		t.Errorf("fresh runtime should have 0 turns")
	}
	if _, ok := st["sessions"]; !ok {
		t.Errorf("status should include sessions")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if firstNonEmpty("", "", "x", "y") != "x" {
		t.Errorf("firstNonEmpty wrong")
	}
	if firstNonEmpty() != "" {
		t.Errorf("empty args should give empty")
	}
}
