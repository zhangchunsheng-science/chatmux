// Package agent is the runtime that binds chats to model agents: it
// consumes inbound messages, serializes turns per chat, streams model
// output back to the chat. Agents come in two flavors: API agents that
// call a provider directly, and CLI agents that drive a headless coding
// CLI (claude/codex/generic) with its own conversational memory.
package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/cliagent"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/provider"
	"github.com/zhangchunsheng-science/chatmux/internal/session"
	"github.com/zhangchunsheng-science/chatmux/internal/usage"
)

// Runtime drives all agents.
type Runtime struct {
	cfg      *config.Config
	registry *provider.Registry
	sessions *session.Store
	chans    map[string]channel.Channel
	cli      *cliagent.Runner
	usage    *usage.Logger
	dataDir  string

	mu    sync.Mutex
	turns map[session.Key]*turn // in-flight turn per chat
}

// turn is one in-flight model call for a chat.
type turn struct {
	cancel  context.CancelFunc
	done    chan struct{}
	agent   *config.AgentConfig
	sender  channel.Sender
	started time.Time
	queued  []provider.Msg // follow-ups arrived mid-turn
}

func NewRuntime(cfg *config.Config, registry *provider.Registry, chans map[string]channel.Channel) *Runtime {
	return &Runtime{
		cfg:      cfg,
		registry: registry,
		sessions: session.NewStore(),
		chans:    chans,
		cli:      cliagent.NewRunner(),
		dataDir:  cfg.DataDir,
	}
}

// SetUsageLogger attaches token accounting (optional).
func (r *Runtime) SetUsageLogger(l *usage.Logger) { r.usage = l }

// CLIRunner exposes the CLI-agent runner (for /new to reset resume ids).
func (r *Runtime) CLIRunner() *cliagent.Runner { return r.cli }

// Handle processes one inbound message. It never blocks the caller for
// the whole model call: if a turn is already running the message is
// queued as a follow-up for the next turn.
func (r *Runtime) Handle(ctx context.Context, msg channel.Message) {
	key := session.Key{Channel: msg.Channel, ChatID: msg.ChatID, ThreadID: msg.ThreadID}

	r.mu.Lock()
	if t, ok := r.turns[key]; ok {
		r.mu.Unlock()
		// /stop must cut the running turn even mid-flight
		if strings.HasPrefix(strings.TrimSpace(msg.Text), "/stop") {
			t.cancel()
			return
		}
		t.queued = append(t.queued, provider.Msg{
			Role: provider.User, Content: msg.Text, Name: msg.UserName,
		})
		// nudge the user that we saw them (best effort, non-fatal)
		if s, err := r.senderFor(msg); err == nil {
			go func() {
				_, _ = s.Send(context.Background(), "已收到，正在处理上一条；再发 /stop 可打断当前任务。", channel.SendOptions{})
			}()
		}
		return
	}
	agentCfg := r.agentFor(msg)
	if agentCfg == nil {
		r.mu.Unlock()
		return
	}
	// Mention gate: in groups the bot answers only when @-mentioned,
	// unless the agent opts out (groupMention=false).
	if !mentionAllowed(agentCfg, msg) {
		r.mu.Unlock()
		return
	}
	sender, err := r.senderFor(msg)
	if err != nil {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	// Slash commands bypass model/CLI entirely.
	if r.dispatchCommand(key, msg, agentCfg, sender) {
		return
	}

	r.mu.Lock()
	if _, busy := r.turns[key]; busy { // raced with another dispatcher
		r.mu.Unlock()
		r.Handle(ctx, msg)
		return
	}
	tctx, cancel := context.WithCancel(context.Background())
	t := &turn{cancel: cancel, done: make(chan struct{}), agent: agentCfg, sender: sender, started: time.Now()}
	r.turns[key] = t
	r.mu.Unlock()

	go func() {
		if t.agent.Cli != nil {
			r.runCliTurn(tctx, key, t, msg)
		} else {
			r.runTurn(tctx, key, t, msg)
		}
		queued := t.queued
		t.queued = nil
		// only remove the turn after runTurn finished, so a follow-up
		// spawned below starts a fresh turn instead of re-queueing
		r.mu.Lock()
		delete(r.turns, key)
		r.mu.Unlock()
		cancel()
		close(t.done)
		if len(queued) > 0 {
			parts := make([]string, len(queued))
			names := ""
			for i, q := range queued {
				parts[i] = q.Content
				if q.Name != "" {
					names += q.Name + " "
				}
			}
			follow := channel.Message{
				Channel:     key.Channel,
				ChatID:      key.ChatID,
				ThreadID:    key.ThreadID,
				ChatType:    msg.ChatType,
				MentionedMe: true,
				UserName:    strings.TrimSpace(names),
				Text:        strings.Join(parts, "\n\n"),
			}
			r.Handle(context.Background(), follow)
		}
	}()
}

// dispatchCommand handles /stop /new /status /model /system /help.
// Returns true when the message was a command (fully handled).
func (r *Runtime) dispatchCommand(key session.Key, msg channel.Message, a *config.AgentConfig, sender channel.Sender) bool {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return false
	}
	cmd, _, _ := strings.Cut(text, "\n")
	cmd, arg, _ := strings.Cut(cmd, " ")
	cmd = strings.ToLower(cmd)
	arg = strings.TrimSpace(arg)

	reply := func(s string) {
		go func() { _, _ = sender.Send(context.Background(), s, channel.SendOptions{Final: true}) }()
	}

	switch cmd {
	case "/stop":
		r.mu.Lock()
		t, ok := r.turns[key]
		r.mu.Unlock()
		if ok {
			t.cancel()
			reply("已请求打断当前任务。")
		} else {
			reply("当前没有在处理的任务。")
		}
	case "/new":
		r.sessions.Reset(key)
		r.cli.Reset(key.String())
		reply("已开启新会话。")
	case "/status":
		r.mu.Lock()
		_, busy := r.turns[key]
		r.mu.Unlock()
		sess := r.sessions.Get(key)
		mode := "api"
		model := firstNonEmpty(sess.Model, a.Model, a.Provider)
		resume := r.cli.ResumeID(key.String())
		if a.Cli != nil {
			mode = "cli:" + a.Cli.Type
			if resume != "" {
				model += " (resume " + resume[:min(8, len(resume))] + ")"
			}
		}
		status := "空闲"
		if busy {
			status = "处理中"
		}
		reply(fmt.Sprintf("chatmux %s · 会话 %s · 模式 %s · 模型 %s · 历史 %d 条",
			status, shortKey(key), mode, model, r.sessions.Count(key)))
	case "/model":
		if arg == "" {
			sess := r.sessions.Get(key)
			cur := firstNonEmpty(sess.Model, a.Model, "(默认)")
			reply(fmt.Sprintf("当前模型: %s\n用法: /model <模型名> 切换", cur))
			break
		}
		r.sessions.Set(key, func(s *session.Session) { s.Model = arg })
		reply("模型已切换为 " + arg + "，下一条消息生效。")
	case "/system":
		if arg == "" {
			sess := r.sessions.Get(key)
			cur := firstNonEmpty(sess.System, a.System, defaultSystemPrompt)
			reply("当前 system 提示词:\n" + truncate(cur, 300) + "\n\n用法: /system <新提示词>；/new 恢复默认")
			break
		}
		r.sessions.Set(key, func(s *session.Session) { s.System = arg })
		reply("system 提示词已更新。")
	case "/help":
		reply(strings.Join([]string{
			"chatmux 命令：",
			"/stop — 打断当前处理中的任务",
			"/new — 开启全新会话（清历史/CLI 会话）",
			"/status — 查看当前会话状态",
			"/model [名称] — 查看/切换模型",
			"/system [提示词] — 查看/覆盖 system 提示词",
			"群里默认需要 @机器人 触发。",
		}, "\n"))
	default:
		// Unknown slash command: let the model see it (agent may explain).
		return false
	}
	return true
}

func (r *Runtime) runTurn(ctx context.Context, key session.Key, t *turn, msg channel.Message) {
	prov, ok := r.registry.Get(providerName(t.agent))
	if !ok {
		_, _ = t.sender.Send(ctx, "模型提供方未配置。", channel.SendOptions{Final: true})
		return
	}

	_ = t.sender.React(ctx, msg.ID, "eyes") // thinking indicator, best effort

	sess := r.sessions.Get(key)
	hist := append([]provider.Msg(nil), sess.History...)

	system := firstNonEmpty(sess.System, t.agent.System, defaultSystemPrompt)

	userMsg := provider.Msg{Role: provider.User, Content: msg.Text, Name: msg.UserName}
	r.attachImages(ctx, msg, &userMsg)

	model := firstNonEmpty(sess.Model, t.agent.Model)

	req := provider.Request{
		Model:       model,
		System:      system,
		Messages:    append(hist, userMsg),
		Temperature: 0.7,
		MaxTokens:   4096,
		APIKey:      t.agent.APIKey,
		BaseURL:     t.agent.BaseURL,
	}

	start := time.Now()
	stream, err := prov.Stream(ctx, req)
	if err != nil {
		_, _ = t.sender.Send(ctx, "模型调用失败: "+err.Error(), channel.SendOptions{Final: true})
		return
	}

	// Stream to the chat: one placeholder message, throttled in-place
	// updates, final replace. Falls back to plain send if replace fails.
	var sb strings.Builder
	placeholderID := ""
	lastUpdate := time.Time{}
	var inTok, outTok int
	for d := range stream {
		if d.Err != nil {
			if placeholderID == "" {
				_, _ = t.sender.Send(ctx, "模型出错: "+d.Err.Error(), channel.SendOptions{Final: true})
			} else {
				_ = t.sender.Replace(ctx, placeholderID, sb.String()+"\n\n[出错: "+d.Err.Error()+"]")
			}
			return
		}
		sb.WriteString(d.Text)
		if d.InputTokens > 0 {
			inTok = d.InputTokens
		}
		if d.OutputTokens > 0 {
			outTok = d.OutputTokens
		}
		if placeholderID == "" && strings.Count(sb.String(), "\n") >= 3 {
			// enough content to show something
			if id, err := t.sender.Send(ctx, sb.String(), channel.SendOptions{}); err == nil {
				placeholderID = id
				lastUpdate = time.Now()
			}
			continue
		}
		if placeholderID != "" && time.Since(lastUpdate) > 2*time.Second {
			_ = t.sender.Replace(ctx, placeholderID, sb.String())
			lastUpdate = time.Now()
		}
	}

	out := sb.String()
	if strings.TrimSpace(out) == "" {
		out = "(空回复)"
	}
	r.usage.Log(usage.Record{
		Agent: t.agent.Name, Mode: "api", Provider: providerName(t.agent), Model: model,
		Channel: key.Channel, ChatID: key.ChatID,
		InputTokens: inTok, OutputTokens: outTok, OutputChars: len(out),
		DurationMs: time.Since(start).Milliseconds(),
	})
	opts := channel.SendOptions{Final: true, ReplyTo: msg.ID, MentionUserIDs: []string{msg.UserID}}
	if placeholderID != "" {
		if err := t.sender.Replace(ctx, placeholderID, out); err == nil {
			_ = t.sender.React(ctx, msg.ID, "white_check_mark")
			r.finish(key, userMsg, out)
			return
		}
		// replace unsupported → fall through to a fresh final send
	}
	if _, err := t.sender.Send(ctx, out, opts); err == nil {
		_ = t.sender.React(ctx, msg.ID, "white_check_mark")
	}
	r.finish(key, userMsg, out)
}

// runCliTurn drives a headless CLI agent (claude/codex/generic). The CLI
// owns its conversational memory via resume ids; chatmux streams progress
// (tool calls + partial text) and posts the final result.
func (r *Runtime) runCliTurn(ctx context.Context, key session.Key, t *turn, msg channel.Message) {
	cfg := *t.agent.Cli
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = t.agent.WorkingDir
	}
	sess := r.sessions.Get(key)
	if m := firstNonEmpty(sess.Model, t.agent.Model); m != "" && cfg.Type != "generic" {
		cfg.Model = m
	}

	prompt := msg.Text
	if msg.UserName != "" {
		prompt = msg.UserName + ":\n" + msg.Text
	}

	_ = t.sender.React(ctx, msg.ID, "eyes")
	start := time.Now()
	events, err := r.cli.Run(ctx, cfg, key.String(), prompt)
	if err != nil {
		_, _ = t.sender.Send(ctx, "启动 CLI 失败: "+err.Error(), channel.SendOptions{Final: true})
		return
	}

	var texts []string // assistant text blocks
	var tools []string // tool-use lines (progress)
	var resultText string
	placeholderID := ""
	lastUpdate := time.Time{}
	var sawErr error

	render := func() string {
		var b strings.Builder
		if len(tools) > 0 {
			keep := tools
			if len(keep) > 5 {
				keep = keep[len(keep)-5:]
			}
			for _, tl := range keep {
				b.WriteString(tl + "\n")
			}
			b.WriteString("\n")
		}
		if len(texts) > 0 {
			tail := texts[len(texts)-1]
			if len(tail) > 2000 {
				tail = "…" + tail[len(tail)-2000:]
			}
			b.WriteString(tail)
		}
		return b.String()
	}

	for ev := range events {
		switch ev.Type {
		case "text":
			texts = append(texts, ev.Text)
		case "tool":
			tools = append(tools, ev.Text)
		case "result":
			resultText = ev.Text
			if ev.Err != nil {
				sawErr = ev.Err
			}
		case "error":
			sawErr = ev.Err
		}
		// throttled progress update
		if placeholderID == "" {
			if s := render(); strings.TrimSpace(s) != "" {
				if id, err := t.sender.Send(ctx, s, channel.SendOptions{}); err == nil {
					placeholderID = id
					lastUpdate = time.Now()
				}
			}
		} else if time.Since(lastUpdate) > 3*time.Second {
			_ = t.sender.Replace(ctx, placeholderID, render())
			lastUpdate = time.Now()
		}
	}

	if ctx.Err() != nil { // /stop
		if placeholderID != "" {
			_ = t.sender.Replace(ctx, placeholderID, render()+"\n\n[已打断]")
		} else {
			_, _ = t.sender.Send(ctx, "[已打断]", channel.SendOptions{Final: true})
		}
		_ = t.sender.React(ctx, msg.ID, "octagonal_sign")
		return
	}

	out := firstNonEmpty(resultText, strings.Join(texts, "\n\n"))
	if sawErr != nil {
		out += "\n\n[CLI 出错: " + sawErr.Error() + "]"
	}
	if strings.TrimSpace(out) == "" {
		out = "(空回复)"
	}
	r.usage.Log(usage.Record{
		Agent: t.agent.Name, Mode: "cli:" + cfg.Type, Model: cfg.Model,
		Channel: key.Channel, ChatID: key.ChatID,
		OutputChars: len(out), DurationMs: time.Since(start).Milliseconds(),
	})
	opts := channel.SendOptions{Final: true, ReplyTo: msg.ID, MentionUserIDs: []string{msg.UserID}}
	if placeholderID != "" {
		if err := t.sender.Replace(ctx, placeholderID, out); err == nil {
			_ = t.sender.React(ctx, msg.ID, "white_check_mark")
			r.finish(key, provider.Msg{Role: provider.User, Content: msg.Text, Name: msg.UserName}, out)
			return
		}
	}
	if _, err := t.sender.Send(ctx, out, opts); err == nil {
		_ = t.sender.React(ctx, msg.ID, "white_check_mark")
	}
	r.finish(key, provider.Msg{Role: provider.User, Content: msg.Text, Name: msg.UserName}, out)
}

// attachImages downloads/collects image attachments into the provider
// message (providers accept http(s) URLs or local paths → data URLs).
func (r *Runtime) attachImages(ctx context.Context, msg channel.Message, out *provider.Msg) {
	ch, ok := r.chans[msg.Channel]
	if !ok {
		return
	}
	dl, ok := ch.(channel.AttachmentDownloader)
	if !ok {
		dl = nil
	}
	for _, att := range msg.Attachments {
		if att.Type != "image" {
			continue
		}
		switch {
		case att.Path != "":
			out.Images = append(out.Images, att.Path)
		case att.URL != "":
			out.Images = append(out.Images, att.URL)
		case att.Key != "" && dl != nil:
			data, mime, err := dl.Download(ctx, att)
			if err != nil || len(data) == 0 {
				continue
			}
			p := filepath.Join(r.dataDir, "attachments", hashOf(att.Key)+"."+extOf(mime))
			if saveFile(p, data) == nil {
				out.Images = append(out.Images, p)
			}
		}
	}
}

// finish records the completed exchange into session history.
func (r *Runtime) finish(key session.Key, userMsg provider.Msg, out string) {
	r.sessions.Append(key, userMsg, provider.Msg{Role: provider.Assistant, Content: out})
}

// Status reports runtime state for the admin endpoint.
func (r *Runtime) Status() map[string]any {
	r.mu.Lock()
	busy := []string{}
	for k := range r.turns {
		busy = append(busy, shortKey(k))
	}
	r.mu.Unlock()
	return map[string]any{
		"sessions":      r.sessions.Len(),
		"turnsInFlight": len(busy),
		"busyAnchors":   busy,
	}
}

func (r *Runtime) senderFor(msg channel.Message) (channel.Sender, error) {
	ch, ok := r.chans[msg.Channel]
	if !ok {
		return nil, fmt.Errorf("unknown channel %s", msg.Channel)
	}
	return ch.SenderFor(msg)
}

func (r *Runtime) agentFor(msg channel.Message) *config.AgentConfig {
	for i := range r.cfg.Agents {
		a := &r.cfg.Agents[i]
		if a.Enabled != nil && !*a.Enabled {
			continue
		}
		for _, rt := range a.Routes {
			if rt.Channel != "" && rt.Channel != "any" && rt.Channel != msg.Channel {
				continue
			}
			if rt.ChatID != "" && rt.ChatID != msg.ChatID && rt.ChatID != msg.ChatType {
				continue
			}
			if rt.ChatType != "" && rt.ChatType != msg.ChatType {
				continue
			}
			if rt.UserID != "" && rt.UserID != msg.UserID {
				continue
			}
			return a
		}
	}
	// default agent by name
	for i := range r.cfg.Agents {
		if r.cfg.Agents[i].Name == r.cfg.DefaultAgent {
			return &r.cfg.Agents[i]
		}
	}
	return nil
}

func providerName(a *config.AgentConfig) string {
	if a.Provider != "" {
		return a.Provider
	}
	return "openai"
}

// mentionAllowed: p2p always; groups require an @ unless the agent
// disables the gate.
func mentionAllowed(a *config.AgentConfig, msg channel.Message) bool {
	if msg.ChatType != "group" || msg.MentionedMe {
		return true
	}
	return a.GroupMention != nil && !*a.GroupMention
}

const defaultSystemPrompt = "You are chatmux, a helpful assistant bridged into a chat app. Reply in the user's language. Keep answers compact: chat UIs, not documents."

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func shortKey(k session.Key) string {
	if k.ThreadID != "" {
		return k.Channel + ":" + k.ChatID + ":" + k.ThreadID
	}
	return k.Channel + ":" + k.ChatID
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
