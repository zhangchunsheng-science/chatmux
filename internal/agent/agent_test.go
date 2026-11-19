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

func TestBuildPromptQuote(t *testing.T) {
	rt, _ := testRuntime()
	msg := channel.Message{Text: "这个问题怎么看", Quoted: &channel.Quoted{UserName: "老王", Text: "房价要涨了吗"}}
	got := rt.buildPrompt(msg)
	if !strings.Contains(got, "[引用 老王 的消息]") || !strings.Contains(got, "房价要涨了吗") || !strings.Contains(got, "这个问题怎么看") {
		t.Errorf("quote prompt wrong: %q", got)
	}
	// no quote → plain text
	if got := rt.buildPrompt(channel.Message{Text: "hi"}); got != "hi" {
		t.Errorf("plain prompt wrong: %q", got)
	}
	// missing username falls back
	got = rt.buildPrompt(channel.Message{Text: "x", Quoted: &channel.Quoted{Text: "y"}})
	if !strings.Contains(got, "引用 某人") {
		t.Errorf("anon quote wrong: %q", got)
	}
}

func TestBgJobLifecycle(t *testing.T) {
	// runtime with a provider that blocks until cancelled, so the job
	// stays alive for /jobs and /stop
	cfg := &config.Config{
		Agents: []config.AgentConfig{{Name: "helper", Provider: "openai"}},
	}
	reg := provider.NewRegistry()
	reg.Register(&blockingProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	key := session.Key{Channel: "feishu", ChatID: "c1"}
	a := &cfg.Agents[0]
	s := &fakeSender{}
	msg := channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "/bg 跑个分析"}

	// /bg registers a job
	if !rt.dispatchCommand(key, msg, a, s) {
		t.Fatalf("/bg should be a command")
	}
	deadline := time.Now().Add(2 * time.Second)
	var found string
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		for id := range rt.jobs {
			found = id
		}
		rt.mu.Unlock()
		if found != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if found == "" {
		t.Fatalf("job not registered")
	}
	// /jobs lists it
	s2 := &fakeSender{}
	rt.dispatchCommand(key, channel.Message{Text: "/jobs", Channel: "feishu"}, a, s2)
	waitContains(t, s2, found)
	// /stop <id> stops it
	s3 := &fakeSender{}
	rt.dispatchCommand(key, channel.Message{Text: "/stop " + found, Channel: "feishu"}, a, s3)
	waitContains(t, s3, "已停止后台任务")
	// stopAll path
	if rt.stopAllJobs() > 1 {
		t.Errorf("stopAll after single stop should find ≤1")
	}
}

func TestFlowSubstitution(t *testing.T) {
	// workflow resolution + {{prev}}/{{input}} substitution, with a fake
	// provider that echoes its prompt.
	noop := false
	cfg := &config.Config{
		Agents: []config.AgentConfig{{Name: "echo", Provider: "openai", Model: "x"}},
		Workflows: []config.WorkflowConfig{{
			Name: "two-step",
			Steps: []config.WorkflowStep{
				{Agent: "echo", Prompt: "第一步: {{input}}"},
				{Agent: "echo", Prompt: "第二步基于: {{prev}}"},
			},
		}},
		DefaultAgent: "echo",
	}
	_ = noop
	reg := provider.NewRegistry()
	reg.Register(&echoProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	ctx := context.Background()
	out, err := rt.runFlow(ctx, session.Key{Channel: "t", ChatID: "c"}, "two-step", "原始输入")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "第一步: 原始输入") {
		t.Errorf("step1 output wrong: %q", out)
	}
}

// echoProvider streams back the last user prompt verbatim.
type echoProvider struct{}

func (e *echoProvider) Name() string { return "openai" }
func (e *echoProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Delta, error) {
	ch := make(chan provider.Delta, 2)
	last := ""
	for _, m := range req.Messages {
		if m.Role == provider.User {
			last = m.Content
		}
	}
	go func() {
		ch <- provider.Delta{Text: last}
		close(ch)
	}()
	return ch, nil
}

// blockingProvider never emits anything until ctx dies.
type blockingProvider struct{}

func (b *blockingProvider) Name() string { return "openai" }
func (b *blockingProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Delta, error) {
	ch := make(chan provider.Delta)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func TestSessionCommands(t *testing.T) {
	rt, key := testRuntime()
	a := &rt.cfg.Agents[0]
	s := &fakeSender{}
	cmd := func(text string) bool {
		return rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: text}, a, s)
	}
	// seed history: one exchange
	rt.sessions.Append(key, provider.Msg{Role: provider.User, Content: "问", Name: "老王"},
		provider.Msg{Role: provider.Assistant, Content: "答"})

	if !cmd("/name 报修群") {
		t.Fatal("/name should dispatch")
	}
	waitContains(t, s, "报修群")

	if !cmd("/history") {
		t.Fatal("/history should dispatch")
	}
	waitContains(t, s, "老王")
	waitContains(t, s, "答")

	if !cmd("/rewind 1") {
		t.Fatal("/rewind should dispatch")
	}
	waitContains(t, s, "剩余历史 0 条")

	if !cmd("/history") {
		t.Fatal("/history 2 should dispatch")
	}
	waitContains(t, s, "还没有历史")
}

func TestRemindCommand(t *testing.T) {
	rt, key := testRuntime()
	a := &rt.cfg.Agents[0]
	s := &fakeSender{}
	// 2s reminder should fire and reach the sender
	if !rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "/remind +2s 喝水"}, a, s) {
		t.Fatal("/remind should dispatch")
	}
	waitContains(t, s, "已设提醒")
	waitContains(t, s, "提醒")
	// bad time gets a usage-style error
	if !rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "/remind 有空 喝水"}, a, s) {
		t.Fatal("/remind bad should dispatch too")
	}
	waitContains(t, s, "看不懂")
}
