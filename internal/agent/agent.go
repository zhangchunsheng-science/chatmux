// Package agent is the runtime that binds chats to model agents: it
// consumes inbound messages, serializes turns per chat, streams model
// output back to the chat.
package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/provider"
	"github.com/zhangchunsheng-science/chatmux/internal/session"
)

// Runtime drives all agents.
type Runtime struct {
	cfg      *config.Config
	registry *provider.Registry
	sessions *session.Store
	chans    map[string]channel.Channel

	mu    sync.Mutex
	turns map[session.Key]*turn // in-flight turn per chat
}

// turn is one in-flight model call for a chat.
type turn struct {
	cancel   context.CancelFunc
	done     chan struct{}
	agent    *config.AgentConfig
	sender   channel.Sender
	queued   []provider.Msg // follow-ups arrived mid-turn
}

func NewRuntime(cfg *config.Config, registry *provider.Registry, chans map[string]channel.Channel) *Runtime {
	return &Runtime{
		cfg:      cfg,
		registry: registry,
		sessions: session.NewStore(),
		chans:    chans,
		turns:    map[session.Key]*turn{},
	}
}

// Handle processes one inbound message. It never blocks the caller for
// the whole model call: if a turn is already running the message is
// queued as a follow-up for the next turn.
func (r *Runtime) Handle(ctx context.Context, msg channel.Message) {
	key := session.Key{Channel: msg.Channel, ChatID: msg.ChatID, ThreadID: msg.ThreadID}

	r.mu.Lock()
	if t, ok := r.turns[key]; ok {
		t.queued = append(t.queued, provider.Msg{
			Role: provider.User, Content: msg.Text, Name: msg.UserName,
		})
		r.mu.Unlock()
		// nudge the user that we saw them (best effort, non-fatal)
		if s, err := r.senderFor(msg); err == nil {
			go func() {
				_, _ = s.Send(context.Background(), "已收到，正在处理上一条，稍后回复这条。", channel.SendOptions{})
			}()
		}
		return
	}
	agentCfg := r.agentFor(msg)
	if agentCfg == nil {
		r.mu.Unlock()
		return
	}
	sender, err := r.senderFor(msg)
	if err != nil {
		r.mu.Unlock()
		return
	}
	tctx, cancel := context.WithCancel(context.Background())
	t := &turn{cancel: cancel, done: make(chan struct{}), agent: agentCfg, sender: sender}
	r.turns[key] = t
	r.mu.Unlock()

	go func() {
		r.runTurn(tctx, key, t, msg)
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
				Channel:  key.Channel,
				ChatID:   key.ChatID,
				ThreadID: key.ThreadID,
				ChatType: "p2p",
				UserName: strings.TrimSpace(names),
				Text:     strings.Join(parts, "\n\n"),
			}
			r.Handle(context.Background(), follow)
		}
	}()
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

	system := t.agent.System
	if sess.System != "" {
		system = sess.System
	}
	if system == "" {
		system = defaultSystemPrompt
	}

	userMsg := provider.Msg{Role: provider.User, Content: msg.Text, Name: msg.UserName}
	for _, att := range msg.Attachments {
		if att.Type != "image" {
			continue
		}
		switch {
		case att.Path != "":
			userMsg.Images = append(userMsg.Images, att.Path)
		case att.URL != "":
			userMsg.Images = append(userMsg.Images, att.URL)
		}
	}

	req := provider.Request{
		Model:       t.agent.Model,
		System:      system,
		Messages:    append(hist, userMsg),
		Temperature: 0.7,
		MaxTokens:   4096,
		APIKey:      t.agent.APIKey,
		BaseURL:     t.agent.BaseURL,
	}

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

// finish records the completed exchange into session history.
func (r *Runtime) finish(key session.Key, userMsg provider.Msg, out string) {
	r.sessions.Append(key, userMsg, provider.Msg{Role: provider.Assistant, Content: out})
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

const defaultSystemPrompt = "You are chatmux, a helpful assistant bridged into a chat app. Reply in the user's language. Keep answers compact: chat UIs, not documents."
