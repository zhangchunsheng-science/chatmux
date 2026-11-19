// Package agent is the runtime that binds chats to model agents: it
// consumes inbound messages, serializes turns per chat, streams model
// output back to the chat. Agents come in two flavors: API agents that
// call a provider directly, and CLI agents that drive a headless coding
// CLI (claude/codex/generic) with its own conversational memory.
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/cliagent"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/cron"
	"github.com/zhangchunsheng-science/chatmux/internal/provider"
	"github.com/zhangchunsheng-science/chatmux/internal/session"
	"github.com/zhangchunsheng-science/chatmux/internal/usage"
)

// Runtime drives all agents.
type Runtime struct {
	cfg        *config.Config
	registry   *provider.Registry
	sessions   *session.Store
	chans      map[string]channel.Channel
	cli        *cliagent.Runner
	usage      *usage.Logger
	dataDir    string
	skills     map[string]string // skill name → prompt markdown
	skillDescs map[string]string // skill name → frontmatter description
	sched      *cron.Scheduler   // set via SetScheduler; nil in tests
	schedCtx   context.Context
	userScheds []userSchedule // /schedule-managed, persisted

	mu     sync.Mutex
	turns  map[session.Key]*turn // in-flight turn per chat
	jobs   map[string]*bgJob     // background jobs by id (bg-N)
	jobSeq int
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

// bgJob is a detached long-running task: the chat stays usable while it
// runs; the result is posted back (and @-mentions its requester).
type bgJob struct {
	id      string
	desc    string
	agent   *config.AgentConfig
	cancel  context.CancelFunc
	started time.Time
	flow    string // non-empty: workflow name instead of a plain prompt
}

func NewRuntime(cfg *config.Config, registry *provider.Registry, chans map[string]channel.Channel) *Runtime {
	return &Runtime{
		cfg:      cfg,
		registry: registry,
		sessions: session.LoadSessions(cfg.DataDir),
		chans:    chans,
		cli:      cliagent.NewRunner(cfg.DataDir),
		dataDir:  cfg.DataDir,
		turns:    map[session.Key]*turn{},
		jobs:     map[string]*bgJob{},
	}
}

// persistSessions snapshots conversations to disk (async, best effort).
func (r *Runtime) persistSessions() {
	if r.dataDir == "" {
		r.dataDir = "data"
	}
	go func() { _ = r.sessions.Save(r.dataDir) }()
}

// Shutdown persists state for a clean restart.
func (r *Runtime) Shutdown() { r.persistSessions() }

// SetSkills installs skill prompts (name → markdown body).
func (r *Runtime) SetSkills(m map[string]string) { r.skills = m }

// SetSkillDescs installs frontmatter descriptions for /skills listings.
func (r *Runtime) SetSkillDescs(m map[string]string) { r.skillDescs = m }

// SetUsageLogger attaches token accounting (optional).
func (r *Runtime) SetUsageLogger(l *usage.Logger) { r.usage = l }

// CLIRunner exposes the CLI-agent runner (for /new to reset resume ids).
func (r *Runtime) CLIRunner() *cliagent.Runner { return r.cli }

// Sessions exposes the session store (for /admin/history).
func (r *Runtime) Sessions() *session.Store { return r.sessions }

// Handle processes one inbound message. It never blocks the caller for
// the whole model call: if a turn is already running the message is
// queued as a follow-up for the next turn.
func (r *Runtime) Handle(ctx context.Context, msg channel.Message) {
	key := session.Key{Channel: msg.Channel, ChatID: msg.ChatID, ThreadID: msg.ThreadID}

	// membership events (bot added/removed): greet or log, no model turn;
	// meeting_* events: announce to the configured meeting chat
	if msg.Event != "" {
		if strings.HasPrefix(msg.Event, "meeting_") {
			r.handleMeetingEvent(msg)
		} else {
			r.handleMemberEvent(msg)
		}
		return
	}

	r.mu.Lock()
	if t, ok := r.turns[key]; ok {
		r.mu.Unlock()
		// commands stay usable mid-turn: /stop cuts the running turn,
		// /jobs //bg //skills etc. answer without queueing
		if strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
			if s, err := r.senderFor(msg); err == nil {
				if strings.HasPrefix(strings.TrimSpace(msg.Text), "/stop") {
					t.cancel()
				}
				if r.dispatchCommand(key, msg, r.agentFor(msg), s) {
					return
				}
			}
		}
		t.queued = append(t.queued, provider.Msg{
			Role: provider.User, Content: msg.Text, Name: msg.UserName,
		})
		// nudge the user that we saw them (best effort, non-fatal)
		if s, err := r.senderFor(msg); err == nil {
			go func() {
				_, _ = s.Send(context.Background(), r.say("queued"), channel.SendOptions{})
			}()
		}
		return
	}
	agentCfg := r.agentFor(msg)
	// per-session agent override (/agent) beats route-based selection
	if name := r.sessions.Get(key).Agent; name != "" {
		if a := r.agentByName(name); a != nil {
			agentCfg = a
		}
	}
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

	r.launchTurn(key, msg, agentCfg, sender)
}

// launchTurn starts a foreground (chat-serializing) turn.
func (r *Runtime) launchTurn(key session.Key, msg channel.Message, agentCfg *config.AgentConfig, sender channel.Sender) {
	r.mu.Lock()
	if _, busy := r.turns[key]; busy { // raced with another dispatcher
		r.mu.Unlock()
		r.Handle(context.Background(), msg)
		return
	}
	tctx, cancel := context.WithCancel(context.Background())
	t := &turn{cancel: cancel, done: make(chan struct{}), agent: agentCfg, sender: sender, started: time.Now()}
	r.turns[key] = t
	r.mu.Unlock()

	go func() {
		r.executeTurn(tctx, key, t, msg, "")
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

// executeTurn runs one model/CLI turn. execKey anchors session history
// and CLI resume ids (background jobs get their own); resultPrefix is
// prepended to the final reply (used by /bg completion notices).
func (r *Runtime) executeTurn(ctx context.Context, execKey session.Key, t *turn, msg channel.Message, resultPrefix string) {
	if t.agent.Cli != nil {
		r.runCliTurn(ctx, execKey, t, msg, resultPrefix)
	} else {
		r.runTurn(ctx, execKey, t, msg, resultPrefix)
	}
}

// dispatchCommand handles the slash command set. Returns true when the
// message was a command (fully handled).
func (r *Runtime) dispatchCommand(key session.Key, msg channel.Message, a *config.AgentConfig, sender channel.Sender) bool {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return false
	}
	cmd, _, _ := strings.Cut(text, "\n")
	cmd, arg, _ := strings.Cut(cmd, " ")
	cmd = strings.ToLower(cmd)
	arg = strings.TrimSpace(arg)
	// multi-line bodies ("/bg 第一行\n第二行") keep everything after the
	// first space, including newlines
	if i := strings.Index(text, " "); i >= 0 {
		arg = strings.TrimSpace(text[i+1:])
	}
	if a == nil { // no agent routed; only meta commands work
		a = &config.AgentConfig{Name: "?"}
	}

	reply := func(s string) {
		go func() { _, _ = sender.Send(context.Background(), s, channel.SendOptions{Final: true}) }()
	}

	switch cmd {
	case "/stop":
		switch {
		case arg == "" || arg == "fg":
			r.mu.Lock()
			t, ok := r.turns[key]
			r.mu.Unlock()
			if ok {
				t.cancel()
				reply(r.say("stop.fg"))
			} else {
				reply(r.say("stop.fg.none"))
			}
		case arg == "bg" || arg == "all":
			n := r.stopAllJobs()
			if n > 0 {
				reply(r.say("stop.bg.done", n))
			} else {
				reply(r.say("stop.bg.none"))
			}
		default:
			if r.stopJob(arg) {
				reply(r.say("stop.job.done", arg))
			} else {
				reply(r.say("stop.job.none", arg))
			}
		}
	case "/bg":
		if arg == "" {
			reply(r.say("bg.usage"))
			break
		}
		r.startBgJob(key, msg, a, sender, arg, "")
	case "/jobs":
		r.mu.Lock()
		jobs := make([]*bgJob, 0, len(r.jobs))
		for _, j := range r.jobs {
			jobs = append(jobs, j)
		}
		r.mu.Unlock()
		if len(jobs) == 0 {
			reply(r.say("jobs.none"))
			break
		}
		lines := []string{r.say("jobs.header")}
		for _, j := range jobs {
			lines = append(lines, r.say("jobs.line", j.id, flowTag(j), truncate(j.desc, 40), time.Since(j.started).Round(time.Second), j.id))
		}
		reply(strings.Join(lines, "\n"))
	case "/new":
		r.sessions.Reset(key)
		r.cli.Reset(key.String())
		r.persistSessions()
		reply(r.say("new.done"))
	case "/name":
		if arg == "" {
			sess := r.sessions.Get(key)
			cur := firstNonEmpty(sess.Title, shortKey(key))
			reply(r.say("name.cur", cur))
			break
		}
		r.sessions.Set(key, func(s *session.Session) { s.Title = truncate(arg, 60) })
		r.persistSessions()
		reply(r.say("name.set", truncate(arg, 60)))
	case "/history":
		n := 6 // last N exchanges (user+assistant pairs shown compactly)
		if arg != "" {
			if v, err := fmt.Sscanf(arg, "%d", &n); v != 1 || err != nil || n < 1 {
				n = 6
			}
			if n > 20 {
				n = 20
			}
		}
		hist := r.sessions.Snapshot(key)
		if len(hist) == 0 {
			reply(r.say("history.none"))
			break
		}
		if n*2 < len(hist) {
			hist = hist[len(hist)-n*2:]
		}
		lines := []string{r.say("history.header", len(hist)/2)}
		for i := 0; i+1 < len(hist); i += 2 {
			u, a := hist[i], hist[i+1]
			who := u.Name
			if who == "" {
				who = r.say("quote.anon")
			}
			lines = append(lines, r.say("history.line", who, truncate(strings.TrimSpace(u.Content), 80), truncate(strings.TrimSpace(a.Content), 200)))
		}
		if len(hist)%2 == 1 { // trailing unpaired user message
			lines = append(lines, r.say("history.tail", truncate(strings.TrimSpace(hist[len(hist)-1].Content), 80)))
		}
		reply(strings.Join(lines, "\n"))
	case "/rewind":
		n := 1
		if arg != "" {
			if v, err := fmt.Sscanf(arg, "%d", &n); v != 1 || err != nil || n < 1 {
				n = 1
			}
			if n > 20 {
				n = 20
			}
		}
		dropped := r.sessions.Rewind(key, n)
		r.persistSessions()
		reply(r.say("rewind.done", dropped, r.sessions.Count(key)))
	case "/retry":
		hist := r.sessions.Snapshot(key)
		if len(hist) < 2 {
			reply(r.say("retry.none"))
			break
		}
		last := hist[len(hist)-2] // user half of the last exchange
		r.sessions.Rewind(key, 1)
		r.persistSessions()
		retry := msg
		retry.Text = last.Content
		retry.UserName = firstNonEmpty(last.Name, msg.UserName)
		go r.launchTurn(key, retry, a, sender)
	case "/remind":
		whenStr, note, _ := strings.Cut(arg, " ")
		at, ok := parseWhen(whenStr)
		if !ok {
			reply(r.say("remind.bad", whenStr))
			break
		}
		note = strings.TrimSpace(note)
		if note == "" {
			reply(r.say("remind.usage"))
			break
		}
		setAt := time.Now()
		delay := at.Sub(setAt)
		reply(r.say("remind.set", truncate(note, 60), humanDur(delay)))
		time.AfterFunc(delay, func() {
			_, _ = sender.Send(context.Background(), r.say("remind.fire", humanDur(time.Since(setAt)), note),
				channel.SendOptions{Final: true, MentionUserIDs: []string{msg.UserID}})
		})
	case "/insight":
		days := 7
		if n, err := strconv.Atoi(strings.TrimSpace(arg)); err == nil && n > 0 && n <= 365 {
			days = n
		}
		aggs, err := usage.SummarizeSince(r.dataDir, time.Now().AddDate(0, 0, -days))
		if err != nil || len(aggs) == 0 {
			reply(r.say("insight.none", days))
			break
		}
		lines := []string{r.say("insight.header", days)}
		for i, a := range aggs {
			if i >= 8 {
				lines = append(lines, r.say("insight.more", len(aggs)-8))
				break
			}
			fb := ""
			if a.FeedbackUp+a.FeedbackDown > 0 {
				fb = " · 👍" + strconv.Itoa(a.FeedbackUp) + "/👎" + strconv.Itoa(a.FeedbackDown)
			}
			errs := ""
			if a.Errors > 0 {
				errs = " · " + strconv.Itoa(a.Errors) + " 失败"
			}
			lines = append(lines, fmt.Sprintf("· %s — %d 次%s · ↑%d ↓%d tokens%s",
				firstNonEmpty(a.Agent, "(未知)"), a.Calls, errs, a.InputTokens, a.OutputTokens, fb))
		}
		reply(strings.Join(lines, "\n"))
	case "/dash":
		listen := "127.0.0.1:8080"
		if r.cfg != nil && r.cfg.HTTP.Listen != "" {
			listen = r.cfg.HTTP.Listen
		}
		reply(r.say("dash.url", "http://"+listen+"/"))
	case "/status":
		r.mu.Lock()
		_, busy := r.turns[key]
		jobsN := len(r.jobs)
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
		status := r.say("status.idle")
		if busy {
			status = r.say("status.busy")
		}
		display := shortKey(key)
		if sess.Title != "" {
			display = sess.Title + " (" + display + ")"
		}
		reply(r.say("status.fmt", status, display, mode, model, r.sessions.Count(key), jobsN))
	case "/model":
		if arg == "" {
			sess := r.sessions.Get(key)
			cur := firstNonEmpty(sess.Model, a.Model, "(默认)")
			reply(r.say("model.cur", cur))
			break
		}
		r.sessions.Set(key, func(s *session.Session) { s.Model = arg })
		r.persistSessions()
		reply(r.say("model.set", arg))
	case "/system":
		if arg == "" {
			sess := r.sessions.Get(key)
			cur := firstNonEmpty(sess.System, a.System, defaultSystemPrompt)
			reply(r.say("system.cur", truncate(cur, 300)))
			break
		}
		r.sessions.Set(key, func(s *session.Session) { s.System = arg })
		r.persistSessions()
		reply(r.say("system.set"))
	case "/skills":
		if len(r.skills) == 0 {
			reply(r.say("skills.none"))
			break
		}
		names := make([]string, 0, len(r.skills))
		for n := range r.skills {
			names = append(names, n)
		}
		sort.Strings(names)
		lines := []string{r.say("skills.header")}
		for _, n := range names {
			d := r.skillDescs[n]
			if d == "" {
				d = truncate(strings.TrimSpace(r.skills[n]), 50)
			}
			lines = append(lines, r.say("skills.line", n, d))
		}
		reply(strings.Join(lines, "\n"))
	case "/skill":
		name, rest, _ := strings.Cut(arg, " ")
		body, ok := r.skills[name]
		if !ok {
			reply(r.say("skill.none", name))
			break
		}
		if strings.TrimSpace(rest) == "" {
			reply(r.say("skill.usage", name))
			break
		}
		// run as a normal turn with the skill prompt prepended
		enriched := msg
		enriched.Text = "【技能 " + name + "】\n" + strings.TrimSpace(body) + "\n\n【用户指令】\n" + rest
		go r.launchTurn(key, enriched, a, sender)
	case "/flows":
		if len(r.cfg.Workflows) == 0 {
			reply(r.say("flows.none"))
			break
		}
		lines := []string{r.say("flows.header")}
		for _, wf := range r.cfg.Workflows {
			d := wf.Desc
			if d == "" {
				d = fmt.Sprintf("%d steps", len(wf.Steps))
			}
			lines = append(lines, r.say("flows.line", wf.Name, d))
		}
		reply(strings.Join(lines, "\n"))
	case "/flow":
		name, rest, _ := strings.Cut(arg, " ")
		var wf *config.WorkflowConfig
		for i := range r.cfg.Workflows {
			if r.cfg.Workflows[i].Name == name {
				wf = &r.cfg.Workflows[i]
				break
			}
		}
		if wf == nil {
			reply(r.say("flow.none", name))
			break
		}
		if strings.TrimSpace(rest) == "" {
			reply(r.say("flow.usage", name))
			break
		}
		r.startBgJob(key, msg, a, sender, rest, name)
	case "/forms":
		if len(r.cfg.Forms) == 0 {
			reply(r.say("forms.none"))
			break
		}
		lines := []string{r.say("forms.header")}
		for _, fc := range r.cfg.Forms {
			d := fc.Desc
			if d == "" {
				d = fmt.Sprintf("%d fields", len(fc.Fields))
			}
			lines = append(lines, r.say("forms.line", fc.Name, d))
		}
		reply(strings.Join(lines, "\n"))
	case "/form":
		if arg == "" {
			reply(r.say("form.usage"))
			break
		}
		var fc *config.FormConfig
		for i := range r.cfg.Forms {
			if r.cfg.Forms[i].Name == arg {
				fc = &r.cfg.Forms[i]
				break
			}
		}
		if fc == nil {
			reply(r.say("form.none", arg))
			break
		}
		fs, ok := sender.(channel.FormSender)
		if !ok {
			reply(r.say("form.unsupported"))
			break
		}
		if err := fs.SendForm(context.Background(), fc.Name, fc.Name, fc.Desc, fc.Fields); err != nil {
			reply(r.say("form.err", err))
			break
		}
		reply(r.say("form.sent", fc.Name))
	case "/agents":
		if len(r.cfg.Agents) == 0 {
			reply(r.say("agents.none"))
			break
		}
		cur := r.sessions.Get(key).Agent
		lines := []string{r.say("agents.header")}
		for _, ag := range r.cfg.Agents {
			if ag.Enabled != nil && !*ag.Enabled {
				continue
			}
			mark := " "
			if ag.Name == firstNonEmpty(cur, r.cfg.DefaultAgent) {
				mark = "●"
			}
			lines = append(lines, r.say("agents.line", mark, ag.Name, agentSummary(&ag)))
		}
		reply(strings.Join(lines, "\n"))
	case "/agent":
		if arg == "" {
			reply(r.say("agent.usage"))
			break
		}
		if arg == "auto" || arg == "default" { // back to route-based
			r.sessions.Set(key, func(s *session.Session) { s.Agent = "" })
			r.persistSessions()
			reply(r.say("agent.auto"))
			break
		}
		if r.agentByName(arg) == nil {
			reply(r.say("agent.none", arg))
			break
		}
		r.sessions.Set(key, func(s *session.Session) { s.Agent = arg })
		r.persistSessions()
		reply(r.say("agent.set", arg))
	case "/schedule":
		r.handleSchedule(key, msg, reply)
	case "/orchestrate":
		return r.handleOrchestrate(key, msg, sender)
	case "/oncall":
		r.handleOncallCommand(reply, strings.TrimSpace(arg))
	case "/help":
		reply(r.say("help"))
	default:
		// dynamic dispatch: /<agent名> <任务> runs that agent as a
		// background job in this chat (botmux multi-bot delegation)
		if name := strings.TrimPrefix(cmd, "/"); name != "" {
			if target := r.agentByName(name); target != nil {
				if strings.TrimSpace(arg) == "" {
					reply(r.say("dispatch.usage", name))
					return true
				}
				r.startBgJob(key, msg, target, sender, arg, "")
				return true
			}
		}
		// Unknown slash command: let the model see it (agent may explain).
		return false
	}
	return true
}

// agentSummary describes an agent for /agents in one short phrase.
func agentSummary(a *config.AgentConfig) string {
	if a.Cli != nil {
		return "cli:" + a.Cli.Type
	}
	if a.Model != "" {
		return firstNonEmpty(a.Provider, "openai") + "/" + a.Model
	}
	return firstNonEmpty(a.Provider, "openai")
}

func flowTag(j *bgJob) string {
	if j.flow != "" {
		return "工作流:" + j.flow
	}
	if j.agent != nil && j.agent.Cli != nil {
		return "cli:" + j.agent.Cli.Type
	}
	return "api"
}

func (r *Runtime) runTurn(ctx context.Context, key session.Key, t *turn, msg channel.Message, resultPrefix string) {
	prov, ok := r.registry.Get(providerName(t.agent))
	if !ok {
		_, _ = t.sender.Send(ctx, r.say("provider.none"), channel.SendOptions{Final: true})
		return
	}

	_ = t.sender.React(ctx, msg.ID, "eyes") // thinking indicator, best effort

	sess := r.sessions.Get(key)
	hist := append([]provider.Msg(nil), sess.History...)

	system := firstNonEmpty(sess.System, t.agent.System, defaultSystemPrompt)

	userMsg := provider.Msg{Role: provider.User, Content: r.buildPrompt(msg), Name: msg.UserName}
	r.attachImages(ctx, msg, &userMsg)
	if vt := r.transcribeAudio(ctx, msg); vt != "" {
		userMsg.Content = strings.TrimSpace(userMsg.Content + "\n" + r.say("voice.tag") + "\n" + vt)
	}

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
		_, _ = t.sender.Send(ctx, r.say("model.fail", err.Error()), channel.SendOptions{Final: true})
		// failed calls are usage too: /insight and the dashboard must
		// see attempts that never produced a reply
		r.usage.Log(usage.Record{
			Agent: t.agent.Name, Mode: "api", Provider: providerName(t.agent), Model: model,
			Channel: msg.Channel, ChatID: msg.ChatID,
			DurationMs: time.Since(start).Milliseconds(),
			Error:      err.Error(),
		})
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
			if ctx.Err() != nil { // cancelled via /stop
				r.reportInterrupt(ctx, t, msg, placeholderID, sb.String())
				return
			}
			if placeholderID == "" {
				_, _ = t.sender.Send(ctx, r.say("model.err", d.Err.Error()), channel.SendOptions{Final: true})
			} else {
				_ = t.sender.Replace(ctx, placeholderID, sb.String()+"\n\n[出错: "+d.Err.Error()+"]")
			}
			// failed calls are usage too: /insight and the dashboard must
			// see attempts that never produced a reply
			r.usage.Log(usage.Record{
				Agent: t.agent.Name, Mode: "api", Provider: providerName(t.agent), Model: model,
				Channel: msg.Channel, ChatID: msg.ChatID,
				OutputChars: len(sb.String()), DurationMs: time.Since(start).Milliseconds(),
				Error: d.Err.Error(),
			})
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

	out := resultPrefix + sb.String()
	if strings.TrimSpace(out) == "" {
		out = r.say("empty.reply")
	}
	r.usage.Log(usage.Record{
		Agent: t.agent.Name, Mode: "api", Provider: providerName(t.agent), Model: model,
		Channel: msg.Channel, ChatID: msg.ChatID,
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

// buildPrompt folds reply-quote context into the user prompt.
func (r *Runtime) buildPrompt(msg channel.Message) string {
	if msg.Quoted == nil || strings.TrimSpace(msg.Quoted.Text) == "" {
		return msg.Text
	}
	who := msg.Quoted.UserName
	if who == "" {
		who = r.say("quote.anon")
	}
	return r.say("quote.fmt", who, truncate(strings.TrimSpace(msg.Quoted.Text), 1000), msg.Text)
}

// transcribeAudio voice-notes an audio attachment into text.
func (r *Runtime) transcribeAudio(ctx context.Context, msg channel.Message) string {
	tc := r.cfg.Transcribe
	if tc == nil || !tc.Enabled {
		return ""
	}
	ch, ok := r.chans[msg.Channel]
	if !ok {
		return ""
	}
	dl, ok := ch.(channel.AttachmentDownloader)
	if !ok {
		return ""
	}
	for _, att := range msg.Attachments {
		if att.Type != "audio" || att.Key == "" {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		data, mime, err := dl.Download(dctx, att)
		cancel()
		if err != nil || len(data) == 0 {
			continue
		}
		p := filepath.Join(r.dataDir, "attachments", hashOf(att.Key)+"."+extOf(mime))
		if saveFile(p, data) != nil {
			continue
		}
		key := firstNonEmpty(tc.APIKey, os.Getenv("OPENAI_API_KEY"))
		if key == "" {
			return ""
		}
		tctx, cancel2 := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel2()
		if txt, err := provider.TranscribeAudio(tctx, nil, p, provider.TranscribeArgs{
			Model: tc.Model, APIKey: key, BaseURL: tc.BaseURL,
		}); err == nil {
			return txt
		}
	}
	return ""
}

// reportInterrupt posts the [已打断] notice for a cancelled turn.
func (r *Runtime) reportInterrupt(ctx context.Context, t *turn, msg channel.Message, placeholderID, partial string) {
	if placeholderID != "" {
		_ = t.sender.Replace(ctx, placeholderID, partial+"\n\n"+r.say("interrupted"))
	} else {
		_, _ = t.sender.Send(ctx, r.say("interrupted"), channel.SendOptions{Final: true})
	}
	_ = t.sender.React(ctx, msg.ID, "octagonal_sign")
}

// runCliTurn drives a headless CLI agent (claude/codex/generic). The CLI
// owns its conversational memory via resume ids; chatmux streams progress
// (tool calls + partial text) and posts the final result.
func (r *Runtime) runCliTurn(ctx context.Context, key session.Key, t *turn, msg channel.Message, resultPrefix string) {
	cfg := *t.agent.Cli
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = t.agent.WorkingDir
	}
	sess := r.sessions.Get(key)
	if m := firstNonEmpty(sess.Model, t.agent.Model); m != "" && cfg.Type != "generic" {
		cfg.Model = m
	}

	prompt := r.buildPrompt(msg)
	if msg.UserName != "" {
		prompt = msg.UserName + ":\n" + prompt
	}

	_ = t.sender.React(ctx, msg.ID, "eyes")
	start := time.Now()
	events, err := r.cli.Run(ctx, cfg, key.String(), prompt)
	if err != nil {
		_, _ = t.sender.Send(ctx, r.say("cli.fail", err.Error()), channel.SendOptions{Final: true})
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
		r.reportInterrupt(ctx, t, msg, placeholderID, render())
		return
	}

	out := resultPrefix + firstNonEmpty(resultText, strings.Join(texts, "\n\n"))
	if sawErr != nil {
		out += "\n\n[CLI 出错: " + sawErr.Error() + "]"
	}
	if strings.TrimSpace(out) == "" {
		out = "(空回复)"
	}
	r.usage.Log(usage.Record{
		Agent: t.agent.Name, Mode: "cli:" + cfg.Type, Model: cfg.Model,
		Channel: msg.Channel, ChatID: msg.ChatID,
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

// finish records the completed exchange into session history and
// snapshots it to disk.
func (r *Runtime) finish(key session.Key, userMsg provider.Msg, out string) {
	r.sessions.Append(key, userMsg, provider.Msg{Role: provider.Assistant, Content: out})
	r.persistSessions()
}

// Status reports runtime state for the admin endpoint.
func (r *Runtime) Status() map[string]any {
	r.mu.Lock()
	busy := []string{}
	for k := range r.turns {
		busy = append(busy, shortKey(k))
	}
	jobs := []map[string]any{}
	for _, j := range r.jobs {
		jobs = append(jobs, map[string]any{
			"id": j.id, "desc": truncate(j.desc, 80), "flow": j.flow,
			"elapsedSec": int64(time.Since(j.started).Seconds()),
		})
	}
	r.mu.Unlock()
	sessions := []map[string]any{}
	for _, si := range r.sessions.Infos() {
		sessions = append(sessions, map[string]any{
			"anchor":   shortKey(si.Key),
			"title":    si.Title,
			"messages": si.Messages,
			"model":    si.Model,
			"agent":    si.Agent,
			"busy":     containsStr(busy, shortKey(si.Key)),
		})
	}
	return map[string]any{
		"sessions":       r.sessions.Len(),
		"turnsInFlight":  len(busy),
		"busyAnchors":    busy,
		"backgroundJobs": jobs,
		"sessionList":    sessions,
	}
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
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

// ---- background jobs ----

// startBgJob launches a detached task (plain prompt or workflow). The
// chat stays free: the job runs on its own anchor and posts the result
// back with an @-mention of its requester.
func (r *Runtime) startBgJob(key session.Key, msg channel.Message, a *config.AgentConfig, sender channel.Sender, task, flowName string) {
	r.mu.Lock()
	r.jobSeq++
	id := fmt.Sprintf("bg-%d", r.jobSeq)
	jctx, cancel := context.WithCancel(context.Background())
	job := &bgJob{id: id, desc: task, agent: a, cancel: cancel, started: time.Now(), flow: flowName}
	r.jobs[id] = job
	r.mu.Unlock()

	_ = sender.React(context.Background(), msg.ID, "rocket")

	go func() {
		execKey := session.Key{Channel: key.Channel, ChatID: key.ChatID + "|job" + id}
		prefix := r.say("bg.done", id, truncate(task, 60), "") + "\n\n"
		if flowName != "" {
			out, err := r.runFlow(jctx, execKey, flowName, task)
			if ctxErr := jctx.Err(); ctxErr != nil {
				_, _ = sender.Send(context.Background(), r.say("bg.stopped", id, flowName), channel.SendOptions{Final: true, MentionUserIDs: []string{msg.UserID}})
			} else if err != nil {
				_, _ = sender.Send(context.Background(), r.say("flow.fail", flowName, err), channel.SendOptions{Final: true, MentionUserIDs: []string{msg.UserID}})
			} else {
				_, _ = sender.Send(context.Background(), r.say("flow.done", flowName, id, out), channel.SendOptions{Final: true, MentionUserIDs: []string{msg.UserID}})
			}
		} else {
			t := &turn{cancel: cancel, done: make(chan struct{}), agent: a, sender: sender, started: time.Now()}
			r.executeTurn(jctx, execKey, t, msg, prefix)
			cancel()
			close(t.done)
		}
		r.mu.Lock()
		delete(r.jobs, id)
		r.mu.Unlock()
	}()

	go func() {
		notice := r.say("bg.started", id, id)
		if flowName != "" {
			notice = r.say("flow.started", flowName, id, id)
		}
		_, _ = sender.Send(context.Background(), notice, channel.SendOptions{Final: true})
	}()
}

func (r *Runtime) stopJob(id string) bool {
	r.mu.Lock()
	j, ok := r.jobs[id]
	r.mu.Unlock()
	if !ok {
		return false
	}
	j.cancel()
	return true
}

func (r *Runtime) stopAllJobs() int {
	r.mu.Lock()
	jobs := make([]*bgJob, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	r.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
	}
	return len(jobs)
}

// ---- workflows ----

// runFlow executes a workflow's step chain synchronously, returning the
// final step output. {{prev}} and {{input}} substitute into prompts.
func (r *Runtime) runFlow(ctx context.Context, execKey session.Key, flowName, input string) (string, error) {
	var wf *config.WorkflowConfig
	for i := range r.cfg.Workflows {
		if r.cfg.Workflows[i].Name == flowName {
			wf = &r.cfg.Workflows[i]
			break
		}
	}
	if wf == nil {
		return "", fmt.Errorf("workflow %q not found", flowName)
	}
	prev := ""
	var lastErr error
	for i, st := range wf.Steps {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		prompt := strings.ReplaceAll(st.Prompt, "{{prev}}", prev)
		prompt = strings.ReplaceAll(prompt, "{{input}}", input)
		a := r.agentByName(st.Agent)
		if a == nil {
			a = r.agentByName(wf.Agent)
		}
		if a == nil {
			a = r.agentByName(r.cfg.DefaultAgent)
		}
		if a == nil {
			return "", fmt.Errorf("step %d: no agent", i+1)
		}
		out, err := r.runOnce(ctx, session.Key{Channel: execKey.Channel, ChatID: execKey.ChatID + fmt.Sprintf("|step%d", i)}, a, prompt)
		if err != nil {
			lastErr = fmt.Errorf("step %d: %w", i+1, err)
			break
		}
		prev = out
	}
	if lastErr != nil {
		return prev, lastErr
	}
	return prev, nil
}

// runOnce does one non-streaming model call (or single CLI turn) and
// returns the full text.
func (r *Runtime) runOnce(ctx context.Context, execKey session.Key, a *config.AgentConfig, prompt string) (string, error) {
	if a.Cli != nil {
		cfg := *a.Cli
		if cfg.WorkingDir == "" {
			cfg.WorkingDir = a.WorkingDir
		}
		if a.Model != "" && cfg.Type != "generic" {
			cfg.Model = a.Model
		}
		events, err := r.cli.Run(ctx, cfg, execKey.String(), prompt)
		if err != nil {
			return "", err
		}
		var texts []string
		var result string
		for ev := range events {
			switch ev.Type {
			case "text":
				texts = append(texts, ev.Text)
			case "result":
				result = ev.Text
			case "error":
				if ctx.Err() == nil {
					return "", ev.Err
				}
			}
		}
		return firstNonEmpty(result, strings.Join(texts, "\n\n")), nil
	}
	prov, ok := r.registry.Get(providerName(a))
	if !ok {
		return "", fmt.Errorf("provider %s 未配置", providerName(a))
	}
	stream, err := prov.Stream(ctx, provider.Request{
		Model: a.Model, System: firstNonEmpty(a.System, defaultSystemPrompt),
		Messages:  []provider.Msg{{Role: provider.User, Content: prompt}},
		MaxTokens: 4096, APIKey: a.APIKey, BaseURL: a.BaseURL,
	})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for d := range stream {
		if d.Err != nil {
			if ctx.Err() != nil {
				return sb.String(), ctx.Err()
			}
			return sb.String(), d.Err
		}
		sb.WriteString(d.Text)
	}
	return sb.String(), nil
}

func (r *Runtime) agentByName(name string) *config.AgentConfig {
	if name == "" {
		return nil
	}
	for i := range r.cfg.Agents {
		if r.cfg.Agents[i].Name == name {
			return &r.cfg.Agents[i]
		}
	}
	return nil
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

// parseWhen understands +30m / +1h30m / 15:30 (today or tomorrow) /
// 2026-11-20 [15:30] (date-only defaults 09:00).
func parseWhen(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "+") {
		d, err := time.ParseDuration(s[1:])
		if err != nil || d <= 0 || d > 365*24*time.Hour {
			return time.Time{}, false
		}
		return time.Now().Add(d), true
	}
	for _, layout := range []struct {
		l        string
		dateOnly bool
	}{
		{"2006-01-02 15:04", false},
		{"2006-01-02T15:04", false},
		{"2006-01-02", true},
	} {
		if t, err := time.ParseInLocation(layout.l, s, time.Local); err == nil {
			if layout.dateOnly {
				t = t.Add(9 * time.Hour)
			} else if !t.After(time.Now()) {
				t = t.Add(24 * time.Hour) // HH:MM already passed → tomorrow
			}
			return t, true
		}
	}
	if t, err := time.ParseInLocation("15:04", s, time.Local); err == nil {
		now := time.Now()
		t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		return t, true
	}
	return time.Time{}, false
}

// humanDur renders a duration compactly (35m / 2h05m / 3d2h).
func humanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}
