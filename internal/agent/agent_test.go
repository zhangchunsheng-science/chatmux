package agent

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/cliagent"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/metrics"
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
	// generous window: stream-start failures retry once after a 2s
	// backoff, so failure notices can land well after the turn starts
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.joined(), substr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for reply containing %q; got:\n%s", substr, s.joined())
}

// testRuntime builds an isolated runtime: DataDir must point at a
// fresh temp dir — with the empty default, NewRuntime falls back to
// the real relative "data/" directory and tests share session state
// through disk, which -shuffle/-count surfaced as cross-test flakes.
func testRuntime(t *testing.T) (*Runtime, session.Key) {
	off := false
	on := true
	cfg := &config.Config{
		DataDir: t.TempDir(),
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
	rt, _ := testRuntime(t)

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
	rt, key := testRuntime(t)
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
	rt, _ := testRuntime(t)
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
	rt, _ := testRuntime(t)
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
		DataDir: t.TempDir(),
		Agents:  []config.AgentConfig{{Name: "helper", Provider: "openai"}},
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
		DataDir: t.TempDir(),
		Agents:  []config.AgentConfig{{Name: "echo", Provider: "openai", Model: "x"}},
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
	rt, key := testRuntime(t)
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
	rt, key := testRuntime(t)
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

func TestAgentOverridePinsAgent(t *testing.T) {
	rt, _ := testRuntime(t)
	// Synthetic producers (docwatch "agent") pin an agent by name on
	// the message itself.
	msg := channel.Message{Channel: "feishu", ChatID: "c1", ChatType: "p2p", MentionedMe: true, Agent: "gated"}
	if a := rt.agentFor(msg); a == nil || a.Name != "gated" {
		t.Fatalf("msg.Agent pin should resolve to gated, got %+v", a)
	}
	// Unknown name falls back to normal routing (chatmux check reports
	// the typo at config time).
	msg.Agent = "ghost"
	if a := rt.agentFor(msg); a == nil || a.Name != "default" {
		t.Fatalf("unknown msg.Agent should fall back to default, got %+v", a)
	}
}

// recordingProvider captures the model each stream was launched with,
// so tests can tell which agent actually handled a turn.
type recordingProvider struct {
	mu     sync.Mutex
	models []string
}

func (p *recordingProvider) Name() string { return "openai" }
func (p *recordingProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Delta, error) {
	p.mu.Lock()
	p.models = append(p.models, req.Model)
	p.mu.Unlock()
	ch := make(chan provider.Delta)
	close(ch)
	return ch, nil
}
func (p *recordingProvider) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.models...)
}

func TestAgentPinBeatsSessionSwitch(t *testing.T) {
	// docwatch "agent" is config-authored routing; the chat's /agent
	// switch is interactive. End to end through Handle, the pin wins.
	cfg := &config.Config{
		DataDir: t.TempDir(),
		Agents: []config.AgentConfig{
			{Name: "default", Provider: "openai", Model: "m-default"},
			{Name: "open", Provider: "openai", Model: "m-open"},
			{Name: "gated", Provider: "openai", Model: "m-gated"},
		},
		DefaultAgent: "default",
	}
	rec := &recordingProvider{}
	reg := provider.NewRegistry()
	reg.Register(rec)
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	rt.chans["feishu"] = &stubChannel{sender: &fakeSender{}}
	key := session.Key{Channel: "feishu", ChatID: "c1"}

	// interactive switch to "open" …
	rt.sessions.Set(key, func(x *session.Session) { x.Agent = "open" })
	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", ChatType: "p2p",
		MentionedMe: true, Text: "hi"})
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(rec.recorded()) >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("only %d turns recorded, want %d: %v", len(rec.recorded()), n, rec.recorded())
	}
	waitFor(1)
	// … a pinned docwatch message still lands on "gated"
	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", ChatType: "p2p",
		MentionedMe: true, Agent: "gated", Text: "doc changed"})
	waitFor(2)
	got := rec.recorded()
	if got[0] != "m-open" {
		t.Fatalf("first turn should use session-switched open, got %v", got)
	}
	if got[1] != "m-gated" {
		t.Fatalf("pinned turn should use gated despite session switch, got %v", got)
	}
}

func TestAgentSwitch(t *testing.T) {
	rt, key := testRuntime(t)
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
		DataDir: t.TempDir(),
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
		DataDir:      t.TempDir(),
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
		DataDir:      t.TempDir(),
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
		DataDir:      t.TempDir(),
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
	rt, key := testRuntime(t)
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
		DataDir:      t.TempDir(),
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

func TestRunOnceAccounting(t *testing.T) {
	key := session.Key{Channel: "feishu", ChatID: "c1|step1"}
	cases := []struct {
		name     string
		prov     provider.Provider
		wantErr  bool
		wantText string
	}{
		{"stream error", &errStreamProvider{}, true, ""},
		{"delta error", &errDeltaProvider{}, true, ""},
		{"success", &echoProvider{}, false, "prompt-x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{
				Agents:       []config.AgentConfig{{Name: "wf", Provider: "openai"}},
				DefaultAgent: "wf", DataDir: dir,
			}
			reg := provider.NewRegistry()
			reg.Register(tc.prov)
			rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
			rt.SetUsageLogger(usage.New(dir))

			out, err := rt.runOnce(context.Background(), key, &cfg.Agents[0], tc.wantText)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				aggs, aerr := usage.Summarize(dir)
				if aerr == nil && len(aggs) == 1 && aggs[0].Calls == 1 {
					gotErr := aggs[0].Errors == 1
					if gotErr != tc.wantErr {
						t.Fatalf("error flag = %v, want %v (%+v)", gotErr, tc.wantErr, aggs[0])
					}
					if err != nil != tc.wantErr {
						t.Fatalf("runOnce err = %v, wantErr %v", err, tc.wantErr)
					}
					if !tc.wantErr && out != tc.wantText {
						t.Fatalf("out = %q, want %q", out, tc.wantText)
					}
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			aggs, _ := usage.Summarize(dir)
			t.Fatalf("call not recorded: %+v", aggs)
		})
	}
}

func TestFmtDur(t *testing.T) {
	if got := fmtDur(340); got != "340ms" {
		t.Errorf("340 → %s", got)
	}
	if got := fmtDur(1520); got != "1.5s" {
		t.Errorf("1520 → %s", got)
	}
	if got := fmtDur(125_000); got != "2m5s" {
		t.Errorf("125000 → %s", got)
	}
}

type dlChannel struct {
	stubChannel
	data []byte
}

func (d *dlChannel) Download(_ context.Context, _ channel.Attachment) ([]byte, string, error) {
	return d.data, "image/png", nil
}

func TestAttachImagesSizeCap(t *testing.T) {
	cfg := &config.Config{
		Agents:       []config.AgentConfig{{Name: "default", Provider: "openai"}},
		DefaultAgent: "default", DataDir: t.TempDir(),
	}
	reg := provider.NewRegistry()
	reg.Register(&echoProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	s := &fakeSender{}
	big := &dlChannel{stubChannel: stubChannel{sender: s}, data: make([]byte, channel.MaxAttachmentBytes+1)}
	rt.chans["feishu"] = big

	var out provider.Msg
	rt.attachImages(context.Background(), channel.Message{
		Channel: "feishu", ChatID: "c1",
		Attachments: []channel.Attachment{{Type: "image", Key: "img_x"}},
	}, &out)
	if len(out.Images) != 0 {
		t.Fatalf("oversized image must be skipped, got %d", len(out.Images))
	}
	// under the cap passes
	big.data = make([]byte, 1024)
	out = provider.Msg{}
	rt.attachImages(context.Background(), channel.Message{
		Channel: "feishu", ChatID: "c1",
		Attachments: []channel.Attachment{{Type: "image", Key: "img_y"}},
	}, &out)
	if len(out.Images) != 1 {
		t.Fatalf("small image must attach, got %d", len(out.Images))
	}
}

func TestBusyAndShutdownDrain(t *testing.T) {
	// Busy() must track in-flight turns so shutdown paths can drain them;
	// Shutdown() snapshots sessions synchronously before exit.
	cfg := &config.Config{
		DataDir:      t.TempDir(),
		Agents:       []config.AgentConfig{{Name: "helper", Provider: "openai"}},
		DefaultAgent: "helper",
	}
	reg := provider.NewRegistry()
	reg.Register(&blockingProvider{})
	dir := t.TempDir()
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	rt.dataDir = dir
	s := &fakeSender{}
	rt.chans["feishu"] = &stubChannel{sender: s}
	key := session.Key{Channel: "feishu", ChatID: "c1"}

	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "慢任务"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rt.Busy() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if rt.Busy() != 1 {
		t.Fatalf("want 1 in-flight turn, got %d", rt.Busy())
	}
	rt.mu.Lock()
	t0 := rt.turns[key]
	rt.mu.Unlock()
	t0.cancel()
	<-t0.done
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rt.Busy() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if rt.Busy() != 0 {
		t.Fatalf("turn finished but Busy() = %d", rt.Busy())
	}

	// Shutdown writes sessions.json synchronously: file exists on return
	rt.sessions.Set(key, func(s *session.Session) { s.Title = "drain-check" })
	rt.Shutdown()
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); err != nil {
		t.Fatalf("Shutdown did not snapshot sessions: %v", err)
	}
}

func TestTurnContextBudget(t *testing.T) {
	near := func(c context.Context, want time.Duration, label string) {
		t.Helper()
		dl, ok := c.Deadline()
		if !ok {
			t.Fatalf("%s: no deadline set", label)
		}
		if d := time.Until(dl); d <= want-time.Minute || d >= want+time.Minute {
			t.Fatalf("%s budget = %v, want ~%v", label, d, want)
		}
	}
	near(mustCtx(turnContext(nil)), 30*time.Minute, "default")
	near(mustCtx(turnContext(&config.AgentConfig{TurnTimeout: "45m"})), 45*time.Minute, "configured")
	near(mustCtx(turnContext(&config.AgentConfig{TurnTimeout: "soon"})), 30*time.Minute, "bad-value")
	near(mustCtx(bgContext(&config.AgentConfig{TurnTimeout: "45m"})), 3*time.Hour, "bg-4x")
}

func mustCtx(c context.Context, _ context.CancelFunc) context.Context { return c }

// hangProvider blocks mid-stream and, when ctx dies, surfaces the
// cancellation as a delta error — what a real transport does (HTTP read
// error / killed CLI child), unlike blockingProvider's clean close.
type hangProvider struct{}

func (h *hangProvider) Name() string { return "openai" }
func (h *hangProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Delta, error) {
	ch := make(chan provider.Delta)
	go func() {
		<-ctx.Done()
		ch <- provider.Delta{Err: ctx.Err()}
		close(ch)
	}()
	return ch, nil
}

func TestTurnWatchdogUnblocksChat(t *testing.T) {
	// a provider that never yields must not hold the chat hostage:
	// the turn budget cancels it, the failure is accounted, and the
	// chat accepts new turns again.
	dir := t.TempDir()
	cfg := &config.Config{
		Agents:       []config.AgentConfig{{Name: "helper", Provider: "openai", TurnTimeout: "150ms"}},
		DefaultAgent: "helper",
		DataDir:      dir,
	}
	reg := provider.NewRegistry()
	reg.Register(&hangProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	rt.SetUsageLogger(usage.New(dir))
	s := &fakeSender{}
	rt.chans["feishu"] = &stubChannel{sender: s}

	wd0, _, _ := metrics.Snapshot()
	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "挂死"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && rt.Busy() > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if rt.Busy() != 0 {
		t.Fatal("watchdog did not release the wedged turn")
	}
	if wd, _, _ := metrics.Snapshot(); wd == wd0 {
		t.Fatal("watchdog fire not counted in metrics")
	}
	// the cancelled turn is accounted as a failed call
	for time.Now().Before(deadline) {
		if aggs, err := usage.Summarize(dir); err == nil && len(aggs) == 1 && aggs[0].Errors == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("watchdog-cancelled turn was not recorded in usage")
}

func TestAttachImagesDownloaderOwnsURL(t *testing.T) {
	// a channel with a downloader owns its URLs (slack url_private needs
	// auth): the attachment must land as a local file, not the raw URL
	rt, key := testRuntime(t)
	rt.dataDir = t.TempDir()
	s := &fakeSender{}
	dl := &dlChannel{stubChannel: stubChannel{sender: s}, data: []byte("imgdata")}
	rt.chans["feishu"] = dl
	msg := channel.Message{Channel: "feishu", ChatID: key.ChatID, UserID: "u1", Text: "看图",
		Attachments: []channel.Attachment{{Type: "image", URL: "https://files.example/f.png"}}}
	out := provider.Msg{Role: provider.User, Content: msg.Text}
	rt.attachImages(context.Background(), msg, &out)
	if len(out.Images) != 1 || out.Images[0] == msg.Attachments[0].URL {
		t.Fatalf("want downloaded local path, got %v", out.Images)
	}
	if _, err := os.Stat(out.Images[0]); err != nil {
		t.Fatalf("downloaded file missing: %v", err)
	}
}

func TestAttachImagesURLFallbackOnDownloadError(t *testing.T) {
	// downloader fails → the URL still goes to the provider as-is
	rt, key := testRuntime(t)
	s := &fakeSender{}
	rt.chans["feishu"] = &errDlChannel{stubChannel: stubChannel{sender: s}}
	msg := channel.Message{Channel: "feishu", ChatID: key.ChatID, UserID: "u1", Text: "看图",
		Attachments: []channel.Attachment{{Type: "image", URL: "https://cdn.example/pub.png"}}}
	out := provider.Msg{Role: provider.User, Content: msg.Text}
	rt.attachImages(context.Background(), msg, &out)
	if len(out.Images) != 1 || out.Images[0] != "https://cdn.example/pub.png" {
		t.Fatalf("want URL fallback, got %v", out.Images)
	}
}

type errDlChannel struct{ stubChannel }

func (d *errDlChannel) Download(_ context.Context, _ channel.Attachment) ([]byte, string, error) {
	return nil, "", fmt.Errorf("boom")
}

func TestTranscribeAudioViaURL(t *testing.T) {
	// audio arriving as a public URL (discord voice) must be fetched and
	// saved even when the channel has no AttachmentDownloader
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/ogg")
		_, _ = w.Write([]byte("oggbytes"))
	}))
	defer srv.Close()

	rt, _ := testRuntime(t)
	rt.dataDir = t.TempDir()
	rt.cfg.Transcribe = &config.TranscribeConfig{Enabled: true, Model: "whisper-1"}
	rt.chans["feishu"] = &stubChannel{sender: &fakeSender{}} // no downloader
	msg := channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1",
		Attachments: []channel.Attachment{{Type: "audio", URL: srv.URL + "/voice.ogg"}}}
	txt := rt.transcribeAudio(context.Background(), msg)
	if txt != "" {
		t.Fatalf("no API key configured — transcription must stay empty, got %q", txt)
	}
	// the fetch+save side effect proves the URL path ran
	found := false
	_ = filepath.Walk(filepath.Join(rt.dataDir, "attachments"), func(p string, _ os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".bin") || strings.HasSuffix(p, ".ogg") {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("URL audio was not fetched/saved")
	}
}

// flakyProvider errors on the first Stream call, succeeds after —
// mirrors a transient network blip at stream start.
type flakyProvider struct {
	calls int
}

func (f *flakyProvider) Name() string { return "openai" }
func (f *flakyProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Delta, error) {
	f.calls++
	if f.calls == 1 {
		return nil, fmt.Errorf("connection reset by peer")
	}
	ch := make(chan provider.Delta, 1)
	ch <- provider.Delta{Text: "好了"}
	close(ch)
	return ch, nil
}

func TestStreamRetryRecovers(t *testing.T) {
	fp := &flakyProvider{}
	// unit: one blip, then success
	_, sr0, _ := metrics.Snapshot()
	ch, err := streamWithRetry(context.Background(), fp, provider.Request{})
	if err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if _, sr, _ := metrics.Snapshot(); sr == sr0 {
		t.Fatal("stream retry not counted in metrics")
	}
	var got string
	for d := range ch {
		got += d.Text
	}
	if got != "好了" || fp.calls != 2 {
		t.Fatalf("got %q after %d calls", got, fp.calls)
	}
}

func TestStreamRetryStillFails(t *testing.T) {
	// always-failing provider: exactly one retry, then the error surfaces
	fp := &errStreamProvider{}
	ch, err := streamWithRetry(context.Background(), fp, provider.Request{})
	if err == nil {
		t.Fatal("want error")
	}
	_ = ch
}

func TestStreamRetryCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fp := &flakyProvider{}
	// cancel while the retry timer is running
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := streamWithRetry(ctx, fp, provider.Request{})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("want ctx error, got %v", err)
	}
	if fp.calls != 1 {
		t.Fatalf("second attempt should not run after cancel, calls=%d", fp.calls)
	}
}

type panicProvider struct{}

func (p *panicProvider) Name() string { return "openai" }
func (p *panicProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Delta, error) {
	panic("model exploded")
}

// A panicking turn must not wedge its chat: before the defer-based
// cleanup the turns entry was never deleted (every future message
// queued forever) and done never closed (Busy/shutdown hung).
func TestTurnPanicRecovers(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{{Name: "helper", Provider: "openai"}}}
	reg := provider.NewRegistry()
	reg.Register(&panicProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	key := session.Key{Channel: "feishu", ChatID: "c1"}

	rt.launchTurn(key, channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "hi"},
		&cfg.Agents[0], &fakeSender{})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		_, busy := rt.turns[key]
		rt.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rt.mu.Lock()
	_, busy := rt.turns[key]
	rt.mu.Unlock()
	if busy {
		t.Fatalf("turn still registered after panic — chat would queue forever")
	}
	if rt.Busy() != 0 {
		t.Fatalf("Busy() = %d after panicked turn, want 0", rt.Busy())
	}
	// the chat accepts a fresh turn again (not the queue-forever path)
	rt.launchTurn(key, channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "again"},
		&cfg.Agents[0], &fakeSender{})
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		_, busy = rt.turns[key]
		rt.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rt.mu.Lock()
	_, busy = rt.turns[key]
	rt.mu.Unlock()
	if busy {
		t.Fatalf("second turn after panic also wedged")
	}
}

// A panicking background job must unregister itself: a leaked entry
// shows as running in /jobs forever and stopAll tries to cancel a dead
// context forever.
func TestBgJobPanicCleansUp(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{{Name: "helper", Provider: "openai"}}}
	reg := provider.NewRegistry()
	reg.Register(&panicProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	key := session.Key{Channel: "feishu", ChatID: "c1"}
	a := &cfg.Agents[0]

	if !rt.dispatchCommand(key, channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "/bg 跑个分析"}, a, &fakeSender{}) {
		t.Fatalf("/bg should be a command")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		n := len(rt.jobs)
		rt.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rt.mu.Lock()
	n := len(rt.jobs)
	rt.mu.Unlock()
	if n != 0 {
		t.Fatalf("panicked bg job still registered (%d), /jobs would show a zombie", n)
	}
}

func TestSlideTurnWindow(t *testing.T) {
	base := int64(100 * time.Second)
	// limit 2: two fresh starts fit, the third is refused and does not
	// consume a slot (the window is unchanged by a refusal)
	w, ok := slideTurnWindow(nil, base, 2)
	if !ok || len(w) != 1 {
		t.Fatalf("first turn: ok=%v win=%v", ok, w)
	}
	w, ok = slideTurnWindow(w, base+int64(time.Second), 2)
	if !ok || len(w) != 2 {
		t.Fatalf("second turn: ok=%v win=%v", ok, w)
	}
	w, ok = slideTurnWindow(w, base+2*int64(time.Second), 2)
	if ok || len(w) != 2 {
		t.Fatalf("third turn must be refused without consuming a slot: ok=%v win=%v", ok, w)
	}
	// entries older than 60s drop out: both starts 61s ago → room again
	old := []int64{base - 61*int64(time.Second), base - 62*int64(time.Second)}
	w, ok = slideTurnWindow(old, base, 2)
	if !ok || len(w) != 1 {
		t.Fatalf("expired entries must be pruned: ok=%v win=%v", ok, w)
	}
}

// countingProvider counts Stream calls and echoes the last user text.
type countingProvider struct{ calls atomic.Int32 }

func (c *countingProvider) Name() string { return "openai" }
func (c *countingProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Delta, error) {
	c.calls.Add(1)
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

func TestTurnRateLimit(t *testing.T) {
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		TurnRatePerMinute: 2,
		Agents:            []config.AgentConfig{{Name: "default", Provider: "openai"}},
		DefaultAgent:      "default",
	}
	reg := provider.NewRegistry()
	cp := &countingProvider{}
	reg.Register(cp)
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	s := &fakeSender{}
	rt.chans["feishu"] = &stubChannel{sender: s}

	for _, txt := range []string{"第一条", "第二条"} {
		rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: txt})
		waitContains(t, s, txt) // echo reply: the turn ran
	}
	// third within the window: refused with a notice, model never called
	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "第三条"})
	waitContains(t, s, "消息太密集")
	if got := cp.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (third turn must never reach the model)", got)
	}
	if n := metrics.RateLimitedTurns(); n < 1 {
		t.Fatalf("chatmux_turns_rate_limited_total counter = %d, want >= 1", n)
	}
	// commands are never rate-limited: /help must answer after the refusal
	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "/help"})
	waitContains(t, s, "/stop")
	if got := cp.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d after /help, still want 2", got)
	}
}

func TestHistoryBudget(t *testing.T) {
	if got := historyBudget(nil); got != 80_000 {
		t.Errorf("nil agent budget = %d, want default", got)
	}
	if got := historyBudget(&config.AgentConfig{}); got != 80_000 {
		t.Errorf("zero override budget = %d, want default", got)
	}
	if got := historyBudget(&config.AgentConfig{MaxContextChars: 6_000}); got != 6_000 {
		t.Errorf("override budget = %d, want 6000", got)
	}
	if got := historyBudget(&config.AgentConfig{MaxContextChars: -5}); got != 80_000 {
		t.Errorf("negative override budget = %d, want default (check flags it)", got)
	}
}

// captureProvider records the request it last served.
type captureProvider struct {
	last provider.Request
	once sync.Once
	seen chan struct{}
}

func (c *captureProvider) Name() string { return "openai" }
func (c *captureProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Delta, error) {
	c.last = req
	select {
	case <-c.seen:
	default:
		close(c.seen)
	}
	ch := make(chan provider.Delta, 2)
	go func() { ch <- provider.Delta{Text: "ok"}; close(ch) }()
	return ch, nil
}

func TestMaxContextCharsOverridesReplayBudget(t *testing.T) {
	cfg := &config.Config{
		DataDir:      t.TempDir(),
		Agents:       []config.AgentConfig{{Name: "small", Provider: "openai", MaxContextChars: 300}},
		DefaultAgent: "small",
	}
	reg := provider.NewRegistry()
	cp := &captureProvider{seen: make(chan struct{})}
	reg.Register(cp)
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	s := &fakeSender{}
	rt.chans["feishu"] = &stubChannel{sender: s}
	key := session.Key{Channel: "feishu", ChatID: "c1"}
	// seed history: three 100-char-pair exchanges (600 chars total,
	// over the 300 budget but satisfiable above the one-exchange floor)
	sess := rt.sessions.Get(key)
	for _, txt := range []string{"aaaa", "bbbb", "cccc"} {
		sess.History = append(sess.History,
			provider.Msg{Role: provider.User, Content: strings.Repeat(txt, 25)},
			provider.Msg{Role: provider.Assistant, Content: strings.Repeat(txt, 25)})
	}
	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "新问题"})
	waitContains(t, s, "ok")
	select {
	case <-cp.seen:
	default:
		t.Fatal("provider never called")
	}
	// history replayed (excluding the new user message) must fit 300 chars
	total := 0
	for i, m := range cp.last.Messages {
		if i == len(cp.last.Messages)-1 {
			continue // current message, never trimmed
		}
		total += len(m.Content)
	}
	if total > 300 {
		t.Fatalf("replayed history = %d chars, want <= 300", total)
	}
	if total == 0 {
		t.Fatal("budget floor must keep at least one exchange")
	}
}

// failProvider errors every stream start (pre-output failure).
type failProvider struct{ calls atomic.Int32 }

func (f *failProvider) Name() string { return "openai" }
func (f *failProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Delta, error) {
	f.calls.Add(1)
	return nil, fmt.Errorf("upstream down")
}

func TestFallbackAgentAnswersWhenPrimaryDown(t *testing.T) {
	cfg := &config.Config{
		DataDir: t.TempDir(),
		Agents: []config.AgentConfig{
			{Name: "main", Provider: "openai", FallbackAgent: "backup"},
			{Name: "backup", Provider: "anthropic"},
		},
		DefaultAgent: "main",
	}
	reg := provider.NewRegistry()
	fp := &failProvider{}
	reg.Register(fp)                       // openai: always down
	reg.Register(&echoProviderAnthropic{}) // anthropic: echoes
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	s := &fakeSender{}
	rt.chans["feishu"] = &stubChannel{sender: s}
	before := metrics.TurnFallbacks()
	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "救场"})
	waitContains(t, s, "救场") // echoed by the backup, not a model.fail notice
	if strings.Contains(s.joined(), "model.fail") && !strings.Contains(s.joined(), "救场") {
		t.Fatal("primary failure must not surface to the user when the backup answers")
	}
	if got := metrics.TurnFallbacks() - before; got < 1 {
		t.Fatalf("fallback jump must count on metrics, delta=%d", got)
	}
}

func TestFallbackChainCappedAndLoopCut(t *testing.T) {
	cfg := &config.Config{
		DataDir: t.TempDir(),
		Agents: []config.AgentConfig{
			{Name: "a", Provider: "openai", FallbackAgent: "b"},
			{Name: "b", Provider: "openai", FallbackAgent: "c"},
			{Name: "c", Provider: "openai", FallbackAgent: "a"}, // loop back to a
		},
		DefaultAgent: "a",
	}
	reg := provider.NewRegistry()
	fp := &failProvider{}
	reg.Register(fp)
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	s := &fakeSender{}
	rt.chans["feishu"] = &stubChannel{sender: s}
	rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: "ping"})
	// three agents × (1 try + 1 stream-retry) × 2s backoff ≈ 6s before the
	// final notice — past the stock waitContains window, so poll longer
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(s.joined(), "模型调用失败") {
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(s.joined(), "模型调用失败") {
		t.Fatalf("final failure notice never arrived; got: %s", s.joined())
	}
	// a → b → c, then the 2-hop cap cuts the loop back to a: 3 agent
	// attempts (2 stream calls each — streamWithRetry retries once)
	if got := fp.calls.Load(); got != 6 {
		t.Fatalf("provider attempts = %d, want 6 (chain cap must cut the loop)", got)
	}
}

// echoProviderAnthropic is the echo behavior on the anthropic name so a
// config can mix providers in one registry.
type echoProviderAnthropic struct{ echoProvider }

func (e *echoProviderAnthropic) Name() string { return "anthropic" }

// wedgeDLChannel mimics a wedged media host: no bytes until the deadline
// kills the request — real HTTP transports honor ctx the same way.
type wedgeDLChannel struct{ stubChannel }

func (w *wedgeDLChannel) Download(ctx context.Context, _ channel.Attachment) ([]byte, string, error) {
	<-ctx.Done()
	return nil, "", ctx.Err()
}

// TestAttachImagesDownloadTimeout pins the per-fetch bound: a wedged image
// download must give up after attachmentDownloadTimeout (not pin the turn
// for a whole turnTimeout) and fall back to handing the public URL over.
func TestAttachImagesDownloadTimeout(t *testing.T) {
	old := attachmentDownloadTimeout
	attachmentDownloadTimeout = 100 * time.Millisecond
	defer func() { attachmentDownloadTimeout = old }()

	cfg := &config.Config{
		Agents:       []config.AgentConfig{{Name: "default", Provider: "openai"}},
		DefaultAgent: "default", DataDir: t.TempDir(),
	}
	reg := provider.NewRegistry()
	reg.Register(&echoProvider{})
	rt := NewRuntime(cfg, reg, map[string]channel.Channel{})
	rt.chans["feishu"] = &wedgeDLChannel{stubChannel{sender: &fakeSender{}}}

	var out provider.Msg
	start := time.Now()
	rt.attachImages(context.Background(), channel.Message{
		Channel: "feishu", ChatID: "c1",
		Attachments: []channel.Attachment{{Type: "image", Key: "k", URL: "https://x/img.png"}},
	}, &out)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wedged download pinned attachImages for %s; per-fetch timeout not wired", elapsed)
	}
	if len(out.Images) != 1 || out.Images[0] != "https://x/img.png" {
		t.Fatalf("failed download must fall back to the public URL, got %v", out.Images)
	}
}

// TestQueuedFollowupsCapped pins the overflow bound: follow-ups arriving
// mid-turn queue up to maxQueuedFollowups and no further — the queue is
// RAM (unbounded before), and a drained mega-batch floods the model's
// replay budget. Overflow drops with a single notice per turn.
func TestQueuedFollowupsCapped(t *testing.T) {
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

	for i := 0; i < maxQueuedFollowups+10; i++ {
		rt.Handle(context.Background(), channel.Message{Channel: "feishu", ChatID: "c1", UserID: "u1", Text: fmt.Sprintf("跟话%d", i)})
	}
	rt.mu.Lock()
	qlen := len(t0.queued)
	rt.mu.Unlock()
	if qlen != maxQueuedFollowups {
		t.Fatalf("queued = %d, want capped at %d", qlen, maxQueuedFollowups)
	}
	waitContains(t, s, "排队消息太多")
	if n := strings.Count(s.joined(), "排队消息太多"); n != 1 {
		t.Fatalf("queue-full notice sent %d times, want exactly 1", n)
	}
	t0.cancel()
	<-t0.done
}
