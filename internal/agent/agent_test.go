package agent

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/cliagent"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/provider"
	"github.com/zhangchunsheng-science/chatmux/internal/session"
	"github.com/zhangchunsheng-science/chatmux/internal/usage"
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

	// /dash replies the dashboard URL
	if !cmd("/dash") {
		t.Fatalf("/dash should be a command")
	}
	waitContains(t, s, "http://127.0.0.1:8080/")

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

func TestAgentSwitch(t *testing.T) {
	rt, key := testRuntime()
	a := &rt.cfg.Agents[0]
	s := &fakeSender{}
	cmd := func(text string) bool {
		return rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: text}, a, s)
	}
	if !cmd("/agents") {
		t.Fatal("/agents should dispatch")
	}
	waitContains(t, s, "gated")
	if !cmd("/agent gated") {
		t.Fatal("/agent should dispatch")
	}
	waitContains(t, s, "已切到 agent gated")
	// override beats route-based selection
	msg := channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "hi", MentionedMe: true, ChatType: "p2p"}
	got := rt.agentFor(msg)
	if got == nil || got.Name != "default" {
		t.Fatalf("route-based selection should still pick default, got %+v", got)
	}
	rt.sessions.Set(key, func(x *session.Session) { x.Agent = "open" })
	// re-run Handle-style resolution manually: override applied in Handle; test the lookup
	if a2 := rt.agentByName("open"); a2 == nil || a2.Name != "open" {
		t.Fatalf("agentByName(open) failed")
	}
	if !cmd("/agent auto") {
		t.Fatal("/agent auto should dispatch")
	}
	waitContains(t, s, "恢复按路由")
}

// tagProvider echoes the model name + last user prompt (distinct per agent).
type tagProvider struct{}

func (e *tagProvider) Name() string { return "openai" }
func (e *tagProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Delta, error) {
	ch := make(chan provider.Delta, 2)
	last := ""
	for _, m := range req.Messages {
		if m.Role == provider.User {
			last = m.Content
		}
	}
	go func() {
		ch <- provider.Delta{Text: "[" + req.Model + "] " + last}
		close(ch)
	}()
	return ch, nil
}

func TestOrchestrate(t *testing.T) {
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{Name: "a1", Provider: "openai", Model: "m1"},
			{Name: "a2", Provider: "openai", Model: "m2"},
			{Name: "merger", Provider: "openai", Model: "mm"},
		},
		DefaultAgent: "a1",
	}
	reg := provider.NewRegistry()
	reg.Register(&tagProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	key := session.Key{Channel: "feishu", ChatID: "c1"}
	s := &fakeSender{}

	if !rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "/orchestrate a1,a2 + merger 做个方案"}, &cfg.Agents[0], s) {
		t.Fatal("/orchestrate not handled")
	}
	waitContains(t, s, "已并行派发给 2 个 agent")
	waitContains(t, s, "【a1】\n[m1] 做个方案")
	waitContains(t, s, "【a2】\n[m2] 做个方案")
	waitContains(t, s, "【汇总 · merger】")

	// unknown agent → clear error, no fan-out
	s2 := &fakeSender{}
	rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", Text: "/orchestrate nope 任务"}, &cfg.Agents[0], s2)
	waitContains(t, s2, "未配置的 agent：nope")

	// missing task → usage
	s3 := &fakeSender{}
	rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", Text: "/orchestrate a1"}, &cfg.Agents[0], s3)
	waitContains(t, s3, "用法: /orchestrate")
}

func TestOncallCommand(t *testing.T) {
	cfg := &config.Config{
		Agents:       []config.AgentConfig{{Name: "default", Provider: "openai"}},
		DefaultAgent: "default",
	}
	rt := NewRuntime(cfg, provider.NewRegistry(), map[string]channel.Channel{})
	key := session.Key{Channel: "feishu", ChatID: "c1"}
	s := &fakeSender{}

	// no roster configured
	rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", Text: "/oncall"}, &cfg.Agents[0], s)
	waitContains(t, s, "没有配置值班表")

	// with roster: anchor today so member 0 is on duty now
	today := time.Now().Format("2006-01-02")
	cfg.Oncall = &config.OncallConfig{Members: []string{"张三", "李四"}, Anchor: today}
	s2 := &fakeSender{}
	rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", Text: "/oncall"}, &cfg.Agents[0], s2)
	waitContains(t, s2, "当前值班：张三")
	waitContains(t, s2, "张三 → 李四")

	s3 := &fakeSender{}
	rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", Text: "/oncall next"}, &cfg.Agents[0], s3)
	waitContains(t, s3, "李四")
}

func TestMemberEventWelcome(t *testing.T) {
	cfg := &config.Config{
		Agents:       []config.AgentConfig{{Name: "default", Provider: "openai"}},
		DefaultAgent: "default",
	}
	rt := NewRuntime(cfg, provider.NewRegistry(), map[string]channel.Channel{})
	key := session.Key{Channel: "feishu", ChatID: "grp1"}
	_ = key
	s := &fakeSender{}
	// wire senderFor: register a channel stub via chans map? Runtime uses
	// r.chans[ msg.Channel ].SenderFor — inject a stub channel.
	rt.chans["feishu"] = &stubChannel{sender: s}

	rt.Handle(context.Background(), channel.Message{
		Channel: "feishu", ChatID: "grp1", ChatType: "group",
		Event: "bot_added", UserName: "event:bot_added",
	})
	waitContains(t, s, "chatmux 机器人")

	rt.Handle(context.Background(), channel.Message{
		Channel: "feishu", ChatID: "grp1", ChatType: "group",
		Event: "bot_removed", UserName: "event:bot_removed",
	})
	// removal: no additional send (still exactly 1 message)
	if n := len(s.sent); n != 1 {
		t.Fatalf("bot_removed should not send, got %d msgs", n)
	}
}

type stubChannel struct{ sender channel.Sender }

func (c *stubChannel) Name() string { return "feishu" }
func (c *stubChannel) Start(ctx context.Context, out chan<- channel.Message) error {
	<-ctx.Done()
	return ctx.Err()
}
func (c *stubChannel) SenderFor(channel.Message) (channel.Sender, error) { return c.sender, nil }
func (c *stubChannel) Healthy() bool                                     { return true }

func TestMeetingEventAnnounce(t *testing.T) {
	cfg := &config.Config{
		Agents:       []config.AgentConfig{{Name: "default", Provider: "openai"}},
		DefaultAgent: "default",
		Meeting:      &config.MeetingConfig{Channel: "feishu", ChatID: "ops"},
	}
	rt := NewRuntime(cfg, provider.NewRegistry(), map[string]channel.Channel{})
	s := &fakeSender{}
	rt.chans["feishu"] = &stubChannel{sender: s}

	rt.Handle(context.Background(), channel.Message{
		Channel: "feishu", ChatID: "mtg1", ChatType: "group",
		Event: "meeting_started", UserName: "event:meeting_started",
		Text: "📞 会议开始：周会",
	})
	waitContains(t, s, "会议开始：周会")

	// no meeting config → usage-only, no send
	rt2 := NewRuntime(&config.Config{
		Agents:       []config.AgentConfig{{Name: "default", Provider: "openai"}},
		DefaultAgent: "default",
	}, provider.NewRegistry(), map[string]channel.Channel{})
	s2 := &fakeSender{}
	rt2.chans["feishu"] = &stubChannel{sender: s2}
	rt2.Handle(context.Background(), channel.Message{
		Channel: "feishu", ChatID: "mtg1", Event: "meeting_recording",
		Text: "🎬 会议录制已就绪：周会",
	})
	if n := len(s2.sent); n != 0 {
		t.Fatalf("without meeting config should not send, got %d", n)
	}
}

func TestInsightCommand(t *testing.T) {
	rt, key := testRuntime()
	rt.dataDir = t.TempDir()
	ul := usage.New(rt.dataDir)
	ul.Log(usage.Record{Agent: "default", Mode: "api", InputTokens: 100, OutputTokens: 200})
	ul.Close()
	a := &rt.cfg.Agents[0]
	s := &fakeSender{}
	if !rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", Text: "/insight"}, a, s) {
		t.Fatal("/insight should be a command")
	}
	waitContains(t, s, "default — 1 次")
	waitContains(t, s, "↑100 ↓200")

	// error rows surface as 失败 in the same line
	ul2 := usage.New(rt.dataDir)
	ul2.Log(usage.Record{Agent: "default", Mode: "api", Error: "boom"})
	ul2.Close()
	s3 := &fakeSender{}
	rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", Text: "/insight"}, a, s3)
	waitContains(t, s3, "2 次 · 1 失败")

	// empty window
	s2 := &fakeSender{}
	rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", Text: "/insight 0"}, a, s2)
	// 0 is not a valid day count → falls back to 7 (still data) — just assert no crash
	_ = s2
}

func TestFailedCallLogged(t *testing.T) {
	cases := []struct {
		name    string
		prov    provider.Provider
		wantMsg string
	}{
		// Stream() itself rejects the request (bad key, network): model.fail path
		{"stream error", &errStreamProvider{}, "模型调用失败"},
		// Stream opens but a delta carries Err: model.err path
		{"delta error", &errDeltaProvider{}, "模型出错"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{
				Agents:       []config.AgentConfig{{Name: "default", Provider: "openai"}},
				DefaultAgent: "default",
				DataDir:      dir,
			}
			reg := provider.NewRegistry()
			reg.Register(tc.prov)
			rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
			rt.SetUsageLogger(usage.New(dir))
			s := &fakeSender{}
			rt.chans["feishu"] = &stubChannel{sender: s}

			rt.Handle(context.Background(), channel.Message{
				Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "你好",
			})
			waitContains(t, s, tc.wantMsg)
			// the failed attempt must appear in usage with error set
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if aggs, err := usage.Summarize(dir); err == nil && len(aggs) == 1 &&
					aggs[0].Calls == 1 && aggs[0].Errors == 1 {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatal("failed call was not recorded in usage")
		})
	}
}

type errStreamProvider struct{}

func (e *errStreamProvider) Name() string { return "openai" }
func (e *errStreamProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Delta, error) {
	return nil, fmt.Errorf("401 unauthorized")
}

type errDeltaProvider struct{}

func (e *errDeltaProvider) Name() string { return "openai" }
func (e *errDeltaProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Delta, error) {
	ch := make(chan provider.Delta, 1)
	go func() {
		ch <- provider.Delta{Err: fmt.Errorf("401 unauthorized")}
		close(ch)
	}()
	return ch, nil
}

func TestQueuedNudgeOnce(t *testing.T) {
	// a burst of follow-ups mid-turn must produce exactly one "queued"
	// ack, not one per message
	cfg := &config.Config{
		Agents:       []config.AgentConfig{{Name: "helper", Provider: "openai"}},
		DefaultAgent: "helper",
	}
	reg := provider.NewRegistry()
	reg.Register(&blockingProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	s := &fakeSender{}
	rt.chans["feishu"] = &stubChannel{sender: s}
	key := session.Key{Channel: "feishu", ChatID: "c1"}

	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "慢任务"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		_, busy := rt.turns[key]
		rt.mu.Unlock()
		if busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rt.mu.Lock()
	t0, busy := rt.turns[key]
	rt.mu.Unlock()
	if !busy {
		t.Fatal("turn not in flight")
	}
	for i := 0; i < 5; i++ {
		rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1",
			Text: fmt.Sprintf("补充 %d", i)})
	}
	time.Sleep(300 * time.Millisecond)
	t0.cancel()
	<-t0.done

	acks := 0
	s.mu.Lock()
	sent := append([]string(nil), s.sent...)
	s.mu.Unlock()
	for _, m := range sent {
		if strings.Contains(m, "正在处理上一条") {
			acks++
		}
	}
	if acks != 1 {
		t.Fatalf("want exactly 1 queued ack for a 5-message burst, got %d", acks)
	}
	// the drained follow-ups must relaunch a fresh turn
	deadline2 := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline2) {
		rt.mu.Lock()
		t1, busy := rt.turns[key]
		rt.mu.Unlock()
		if busy && t1 != t0 {
			t1.cancel()
			<-t1.done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("drained follow-ups did not relaunch a turn")
}

func TestTrimToBudget(t *testing.T) {
	small := []provider.Msg{
		{Role: provider.User, Content: "hi"},
		{Role: provider.Assistant, Content: "hello"},
	}
	if got := trimToBudget(small, 1000); len(got) != 2 {
		t.Fatalf("small history must pass through, got %d", len(got))
	}
	huge := []provider.Msg{
		{Role: provider.User, Content: strings.Repeat("a", 90_000)},
		{Role: provider.Assistant, Content: strings.Repeat("b", 90_000)},
		{Role: provider.User, Content: "问题"},
		{Role: provider.Assistant, Content: "回答"},
	}
	got := trimToBudget(huge, 80_000)
	if len(got) != 2 || got[0].Content != "问题" {
		t.Fatalf("oversized pairs must drop oldest first, kept %+v", len(got))
	}
	// all oversized: still keeps the newest exchange
	big := []provider.Msg{
		{Role: provider.User, Content: strings.Repeat("x", 60_000)},
		{Role: provider.Assistant, Content: strings.Repeat("y", 60_000)},
	}
	if got := trimToBudget(big, 80_000); len(got) != 2 {
		t.Fatalf("must always keep one exchange, got %d", len(got))
	}
}

// syncedBuffer makes bytes.Buffer safe to read while the logger writes.
type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestTurnLogged(t *testing.T) {
	cfg := &config.Config{
		Agents:       []config.AgentConfig{{Name: "default", Provider: "openai"}},
		DefaultAgent: "default",
	}
	reg := provider.NewRegistry()
	reg.Register(&echoProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	s := &fakeSender{}
	rt.chans["feishu"] = &stubChannel{sender: s}

	lb := &syncedBuffer{}
	log.SetOutput(lb)
	defer log.SetOutput(os.Stderr)

	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "ping"})
	waitContains(t, s, "ping")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if out := lb.String(); strings.Contains(out, "turn status=ok agent=default") &&
			strings.Contains(out, "chat=feishu/c1") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("turn log line missing: %q", lb.String())
}

type fakeCLIRunner struct {
	runErr error
	events []cliagent.Event
}

func (f *fakeCLIRunner) Run(_ context.Context, _ cliagent.Config, _, _ string) (<-chan cliagent.Event, error) {
	if f.runErr != nil {
		return nil, f.runErr
	}
	ch := make(chan cliagent.Event, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}
func (f *fakeCLIRunner) Reset(string)           {}
func (f *fakeCLIRunner) ResumeID(string) string { return "" }

func TestCliTurnAccounting(t *testing.T) {
	cases := []struct {
		name     string
		runner   *fakeCLIRunner
		wantErrs int
	}{
		{"launch fail", &fakeCLIRunner{runErr: fmt.Errorf("claude: not found")}, 1},
		{"stream error", &fakeCLIRunner{events: []cliagent.Event{
			{Type: "text", Text: "部分输出"},
			{Type: "error", Err: fmt.Errorf("tool crashed")},
		}}, 1},
		{"success", &fakeCLIRunner{events: []cliagent.Event{
			{Type: "text", Text: "完成"},
			{Type: "result", Text: "最终答案"},
		}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{
				Agents: []config.AgentConfig{{
					Name: "coder", Provider: "openai",
					Cli: &cliagent.Config{Type: "claude"},
				}},
				DefaultAgent: "coder",
				DataDir:      dir,
			}
			reg := provider.NewRegistry()
			rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
			rt.cli = tc.runner
			rt.SetUsageLogger(usage.New(dir))
			s := &fakeSender{}
			rt.chans["feishu"] = &stubChannel{sender: s}
			rt.Handle(context.Background(), channel.Message{
				Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "跑一下",
			})
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				aggs, err := usage.Summarize(dir)
				if err == nil && len(aggs) == 1 && aggs[0].Calls == 1 && aggs[0].Errors == tc.wantErrs {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			aggs, _ := usage.Summarize(dir)
			t.Fatalf("usage wrong: %+v", aggs)
		})
	}
}
