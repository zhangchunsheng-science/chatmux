// chatmux: one binary, many chat platforms, one brain.
//
// Bridges IM channels (Feishu, DingTalk, WeCom, Telegram, Discord,
// Slack, ...) to AI model providers (OpenAI-compatible, Anthropic) or
// headless CLI agents (claude/codex), with streaming replies, per-thread
// sessions, durable dedup, cron schedules and durable usage accounting.
//
// Server:   chatmux -config chatmux.json
// Client:   chatmux status|stop|send|ask|mcp|version|update|fleet [-config chatmux.json]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/agent"
	"github.com/zhangchunsheng-science/chatmux/internal/ask"
	"github.com/zhangchunsheng-science/chatmux/internal/bus"
	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/dingtalk"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/discord"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/feishu"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/slack"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/telegram"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/web"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/wecom"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/cron"
	"github.com/zhangchunsheng-science/chatmux/internal/docwatch"
	"github.com/zhangchunsheng-science/chatmux/internal/fleet"
	"github.com/zhangchunsheng-science/chatmux/internal/mcp"
	"github.com/zhangchunsheng-science/chatmux/internal/metrics"
	"github.com/zhangchunsheng-science/chatmux/internal/oncall"
	"github.com/zhangchunsheng-science/chatmux/internal/provider"
	"github.com/zhangchunsheng-science/chatmux/internal/session"
	"github.com/zhangchunsheng-science/chatmux/internal/update"
	"github.com/zhangchunsheng-science/chatmux/internal/usage"
	"github.com/zhangchunsheng-science/chatmux/internal/version"
)

// Channel restart supervision: a crashing channel (e.g. bad credentials
// rejected at handshake) is retried with exponential backoff instead of a
// fixed 5s loop that hammers the upstream API and floods the log.
const (
	minChannelBackoff = 5 * time.Second
	maxChannelBackoff = 5 * time.Minute
	healthyChannelRun = 10 * time.Minute // a run at least this long resets the backoff
)

func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > maxChannelBackoff {
		return maxChannelBackoff
	}
	return next
}

func main() {
	cfgPath := flag.String("config", "chatmux.json", "path to chatmux.json")
	flag.Parse()

	if len(flag.Args()) > 0 {
		os.Exit(runClient(flag.Args(), *cfgPath))
	}
	os.Exit(runServer(*cfgPath))
}

// ---- client mode: status / stop / send ----

func runClient(args []string, cfgPath string) int {
	cmd := args[0]
	// version/update/fleet need no gateway config
	switch cmd {
	case "version":
		fmt.Printf("chatmux v%s (%s/%s)\n", version.Version, runtime.GOOS, runtime.GOARCH)
		return 0
	case "check":
		// `chatmux check [path]` — default path from -config
		path := cfgPath
		if len(args) > 1 {
			path = args[1]
		}
		return runCheck(path)
	case "update":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		res, err := update.Apply(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "更新失败:", err)
			return 1
		}
		if res.UpToDate {
			fmt.Printf("已是最新版 v%s。\n", res.From)
			return 0
		}
		fmt.Printf("已更新 v%s → v%s（重启 chatmux 生效）。\n", res.From, res.To)
		return 0
	case "fleet":
		return runFleet(args[1:])
	}
	if cmd != "status" && cmd != "stop" && cmd != "send" && cmd != "ask" && cmd != "mcp" {
		fmt.Fprintf(os.Stderr, "unknown command %q (want status|stop|send|ask|mcp|version|update|fleet)\n", cmd)
		return 2
	}
	// accept -config after the subcommand too: chatmux status -config x.json
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		if rest[i] == "-config" && i+1 < len(rest) {
			cfgPath = rest[i+1]
			rest = append(rest[:i:i], rest[i+2:]...)
			break
		}
		if strings.HasPrefix(rest[i], "-config=") {
			cfgPath = strings.TrimPrefix(rest[i], "-config=")
			rest = append(rest[:i:i], rest[i+1:]...)
			break
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	base := "http://" + cfg.HTTP.Listen
	token := cfg.HTTP.AdminToken

	switch cmd {
	case "status":
		body, code, err := adminReq(http.MethodGet, base+"/admin/status", token, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if code != 200 {
			fmt.Fprintf(os.Stderr, "HTTP %d\n%s\n", code, body)
			return 1
		}
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, body, "", "  ")
		fmt.Println(pretty.String())
	case "stop":
		_, code, err := adminReq(http.MethodPost, base+"/admin/stop", token, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if code != 200 {
			fmt.Printf("HTTP %d\n", code)
			return 1
		}
		fmt.Println("chatmux 停止请求已发出。")
	case "send":
		fs := flag.NewFlagSet("send", flag.ExitOnError)
		to := fs.String("to", "", "channel:chatId[:threadId]")
		file := fs.String("file", "", "local file to upload (optional, caption = 文本)")
		fs.Parse(rest)
		text := strings.Join(fs.Args(), " ")
		if *to == "" || (text == "" && *file == "") {
			fmt.Fprintln(os.Stderr, `usage: chatmux send -to channel:chatId[:threadId] [-file 路径] 文本`)
			return 2
		}
		parts := strings.SplitN(*to, ":", 3)
		payload := map[string]string{"channel": parts[0], "chatId": parts[1], "text": text}
		if *file != "" {
			payload["file"] = *file
		}
		if len(parts) == 3 {
			payload["threadId"] = parts[2]
		}
		b, _ := json.Marshal(payload)
		body, code, err := adminReq(http.MethodPost, base+"/admin/send", token, b)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if code != 200 {
			fmt.Fprintf(os.Stderr, "HTTP %d\n%s\n", code, body)
			return 1
		}
		fmt.Println("已发送。")
	case "ask":
		fs := flag.NewFlagSet("ask", flag.ExitOnError)
		to := fs.String("to", "", "channel:chatId[:threadId]")
		_ = fs.String("opt", "", "fixed option (repeatable)")
		timeout := fs.Int("timeout", 600, "timeout seconds")
		fs.Parse(rest)
		// collect repeated -opt flags in original order
		var opts []string
		raw := rest
		for i := 0; i < len(raw); i++ {
			if raw[i] == "-opt" && i+1 < len(raw) {
				opts = append(opts, raw[i+1])
			}
		}
		question := strings.Join(fs.Args(), " ")
		if *to == "" || question == "" {
			fmt.Fprintln(os.Stderr, `usage: chatmux ask -to channel:chatId [-opt A -opt B] [-timeout 600] 问题`)
			return 2
		}
		parts := strings.SplitN(*to, ":", 3)
		payload := map[string]any{"channel": parts[0], "chatId": parts[1], "question": question, "timeoutSec": *timeout}
		if len(opts) > 0 {
			payload["options"] = opts
		}
		if len(parts) == 3 {
			payload["threadId"] = parts[2]
		}
		b, _ := json.Marshal(payload)
		fmt.Println("已向聊天提问，等待回答…（Ctrl-C 放弃）")
		body, code, err := adminReq(http.MethodPost, base+"/admin/ask", token, b)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if code != 200 {
			fmt.Fprintf(os.Stderr, "HTTP %d\n%s\n", code, body)
			return 1
		}
		var out struct {
			Answer string `json:"answer"`
		}
		_ = json.Unmarshal(body, &out)
		fmt.Println(out.Answer)
	case "mcp":
		// stdio MCP server proxying this gateway — register as
		//   claude mcp add chatmux -- chatmux mcp -config chatmux.json
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		log.SetOutput(os.Stderr) // keep stdout protocol-clean
		if err := mcp.New(base, token).Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "mcp:", err)
			return 1
		}
	}
	return 0
}

// runFleet: chatmux fleet list|status <名>|send <名> -to ch:chat 文本|ask <名> -to ch:chat 问题
// Fleet file: -fleet 路径（默认 ~/.chatmux/fleet.json），格式
// {"instances":[{"name":"x","baseUrl":"http://...","token":"..."}]}
func runFleet(args []string) int {
	fleetPath := os.Getenv("CHATMUX_FLEET")
	if fleetPath == "" {
		home, _ := os.UserHomeDir()
		fleetPath = filepath.Join(home, ".chatmux", "fleet.json")
	}
	// scan out -fleet/-fleet= before the subcommand
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-fleet" && i+1 < len(args) {
			fleetPath = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-fleet=") {
			fleetPath = strings.TrimPrefix(args[i], "-fleet=")
			continue
		}
		rest = append(rest, args[i])
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, `usage: chatmux fleet [-fleet 路径] list | status <实例> | send <实例> -to channel:chatId 文本 | ask <实例> -to channel:chatId [-opt A] 问题`)
		return 2
	}
	fc, err := fleet.Load(fleetPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "fleet 文件不存在？先建一个：{\"instances\":[{\"name\":\"local\",\"baseUrl\":\"http://127.0.0.1:8080\",\"token\":\"...\"}]}")
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	sub := rest[0]
	rest = rest[1:]
	switch sub {
	case "list":
		for _, s := range fc.List(ctx) {
			if !s.Reachable {
				fmt.Printf("%-12s ✗ 不可达（%s）\n", s.Name, s.Err)
				continue
			}
			fmt.Printf("%-12s ✓ v%s · 通道 %d/%d · agents %d\n", s.Name, s.Version, s.ChannelsUp, s.ChannelsAll, s.Agents)
		}
		return 0
	case "status":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: chatmux fleet status <实例>")
			return 2
		}
		body, code, err := fc.Call(ctx, rest[0], http.MethodGet, "/admin/status", nil)
		if err != nil || code != 200 {
			fmt.Fprintf(os.Stderr, "HTTP %d err=%v\n", code, err)
			return 1
		}
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, body, "", "  ")
		fmt.Println(pretty.String())
		return 0
	case "send", "ask":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: chatmux fleet "+sub+" <实例> -to channel:chatId [文本/问题]")
			return 2
		}
		name := rest[0]
		fs := flag.NewFlagSet("fleet "+sub, flag.ExitOnError)
		to := fs.String("to", "", "channel:chatId[:threadId]")
		timeout := fs.Int("timeout", 600, "ask timeout seconds")
		_ = fs.String("opt", "", "fixed option (repeatable)")
		fs.Parse(rest[1:])
		text := strings.Join(fs.Args(), " ")
		if *to == "" || strings.TrimSpace(text) == "" {
			fmt.Fprintln(os.Stderr, "usage: chatmux fleet "+sub+" <实例> -to channel:chatId [文本/问题]")
			return 2
		}
		parts := strings.SplitN(*to, ":", 3)
		var path string
		var payload any
		if sub == "send" {
			path = "/admin/send"
			payload = map[string]any{"channel": parts[0], "chatId": parts[1], "text": text}
		} else {
			path = "/admin/ask"
			payload = map[string]any{"channel": parts[0], "chatId": parts[1], "question": text, "timeoutSec": *timeout}
		}
		if len(parts) == 3 {
			payload.(map[string]any)["threadId"] = parts[2]
		}
		if sub == "ask" {
			var opts []string
			raw := rest[1:]
			for i := 0; i < len(raw); i++ {
				if raw[i] == "-opt" && i+1 < len(raw) {
					opts = append(opts, raw[i+1])
				}
			}
			if len(opts) > 0 {
				payload.(map[string]any)["options"] = opts
			}
		}
		body, code, err := fc.Call(ctx, name, http.MethodPost, path, payload)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if code != 200 {
			fmt.Fprintf(os.Stderr, "HTTP %d\n%s\n", code, body)
			return 1
		}
		if sub == "send" {
			fmt.Println("已发送。")
			return 0
		}
		var out struct {
			Answer string `json:"answer"`
		}
		_ = json.Unmarshal(body, &out)
		fmt.Println(out.Answer)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown fleet subcommand %q (want list|status|send|ask)\n", sub)
		return 2
	}
}

func adminReq(method, url, token string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}

// ---- server mode ----

// channelFactories maps config type → constructor; shared by the server
// boot and `chatmux check`.
var channelFactories = map[string]func(opts json.RawMessage) (channel.Channel, error){
	"feishu":   feishu.New,
	"dingtalk": dingtalk.New,
	"wecom":    wecom.New,
	"telegram": telegram.New,
	"discord":  discord.New,
	"slack":    slack.New,
}

// runCheck validates a config file end to end without starting anything:
// semantic checks (agents/routes/cron/...) plus real channel construction.
func runCheck(cfgPath string) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	problems, warnings := config.Validate(cfg)
	for i, cc := range cfg.Channels {
		f, ok := channelFactories[cc.Type]
		if !ok {
			problems = append(problems, fmt.Sprintf("channels[%d] %q: unknown type %q", i, cc.Name, cc.Type))
			continue
		}
		if _, err := f(cc.Options); err != nil {
			problems = append(problems, fmt.Sprintf("channels[%d] %q: %v", i, cc.Name, err))
		}
	}
	for _, w := range warnings {
		fmt.Printf("⚠ %s\n", w)
	}
	if len(problems) == 0 {
		fmt.Printf("✓ %s：配置有效（%d 通道 %d agent %d cron）\n", cfgPath, len(cfg.Channels), len(cfg.Agents), len(cfg.Cron))
		return 0
	}
	for _, p := range problems {
		fmt.Printf("✗ %s\n", p)
	}
	fmt.Fprintf(os.Stderr, "%d 个问题\n", len(problems))
	return 1
}

func runServer(cfgPath string) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// ---- providers ----
	registry := provider.NewRegistry()
	registry.Register(&provider.OpenAI{APIKey: os.Getenv("OPENAI_API_KEY")})
	registry.Register(&provider.Anthropic{APIKey: os.Getenv("ANTHROPIC_API_KEY")})

	// ---- channels ----
	factories := channelFactories
	chans := map[string]channel.Channel{}
	var mux http.ServeMux
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ok := true
		for _, ch := range chans {
			if !ch.Healthy() {
				ok = false
			}
		}
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("degraded"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	for _, cc := range cfg.Channels {
		f, ok := factories[cc.Type]
		if !ok {
			log.Fatalf("unknown channel type %q", cc.Type)
		}
		ch, err := f(cc.Options)
		if err != nil {
			log.Fatalf("channel %s: %v", cc.Name, err)
		}
		if n, ok := ch.(interface{ SetName(string) }); ok {
			n.SetName(cc.Name)
		}
		chans[cc.Name] = ch
		if m, ok := ch.(channel.HTTPMounter); ok {
			for path, h := range m.Endpoints() {
				mux.Handle(path, h)
			}
		}
	}
	// web console channel: always on, powers the dashboard chat panel
	webCh := web.New("web")
	chans[webCh.Name()] = webCh

	// ---- runtime ----
	b := bus.New()
	rt := agent.NewRuntime(cfg, registry, chans)
	ul := usage.New(cfg.DataDir)
	rt.SetUsageLogger(ul)
	skillBodies, skillDescs := loadSkills(cfg.SkillsDir)
	rt.SetSkills(skillBodies)
	rt.SetSkillDescs(skillDescs)
	// 👍/👎 feedback (feishu/telegram/discord/slack) → usage log
	for _, ch := range chans {
		name := ch.Name()
		if fh, ok := ch.(interface {
			SetFeedbackHook(func(kind, operator, msgID string))
		}); ok {
			fh.SetFeedbackHook(func(kind, operator, msgID string) {
				log.Printf("feedback %s by %s on %s (%s)", kind, operator, msgID, name)
				ul.Log(usage.Record{
					Agent: "feedback", Mode: "feedback:" + kind,
					Channel: name, ChatID: operator,
				})
			})
		}
	}
	// form card submissions (feishu) → ask answers, else synthetic message
	askBroker := ask.New()
	for _, ch := range chans {
		name := ch.Name()
		if fh, ok := ch.(interface {
			SetFormHandler(func(form, chatID, operator string, values map[string]string))
		}); ok {
			fh.SetFormHandler(func(form, chatID, operator string, values map[string]string) {
				if strings.HasPrefix(form, "chatmux_ask:") {
					log.Printf("ask answered via form by %s in %s", operator, chatID)
					askBroker.ResolveForm(form, values)
					return
				}
				log.Printf("form %s submitted by %s in %s", form, operator, chatID)
				synthetic := channel.Message{
					Channel: name, ChatID: chatID, UserID: operator,
					UserName: operator, MentionedMe: true,
					Text:      formatFormSubmit(cfg, form, operator, values),
					Timestamp: time.Now(),
				}
				go func() { b.Inbound() <- synthetic }()
			})
		}
	}
	in := b.Subscribe()
	go func() {
		for msg := range in {
			// a pending blocking question in this chat eats the next
			// (valid) reply as its answer before the agent sees it
			if askBroker.TryAnswer(msg) {
				continue
			}
			go rt.Handle(context.Background(), msg)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- cron schedules ----
	sched := cron.New()
	for _, cc := range cfg.Cron {
		if cc.Enabled != nil && !*cc.Enabled {
			continue
		}
		if _, chOk := chans[cc.Channel]; !chOk {
			log.Fatalf("cron %s: unknown channel %q", cc.Name, cc.Channel)
		}
		if _, err := cron.Next(time.Now(), cc.Schedule); err != nil {
			log.Fatalf("cron %s: %v", cc.Name, err)
		}
		entry := cc
		sched.Add(cron.Entry{
			Name: entry.Name,
			Spec: entry.Schedule,
			Fire: func(ctx context.Context) {
				log.Printf("cron %s fired → %s:%s", entry.Name, entry.Channel, entry.ChatID)
				rt.Handle(ctx, channel.Message{
					Channel: entry.Channel, ChatID: entry.ChatID, ThreadID: entry.ThreadID,
					ChatType: "p2p", MentionedMe: true,
					UserName: "cron:" + entry.Name, Text: entry.Text,
				})
			},
		})
		log.Printf("cron %s @ %s → %s:%s", cc.Name, cc.Schedule, cc.Channel, cc.ChatID)
	}
	sched.Start(ctx)
	rt.SetScheduler(sched, ctx) // /schedule command + persisted user schedules

	// on-call handover notice: check right after midnight; announce changes
	if cfg.Oncall != nil && len(cfg.Oncall.Members) > 0 && cfg.Oncall.Channel != "" && cfg.Oncall.ChatID != "" {
		if oc, err := oncall.Parse(*cfg.Oncall, time.Now()); err == nil {
			sched.AddLive(ctx, cron.Entry{
				Name: "oncall-handover", Spec: "5 0 * * *",
				Fire: func(ctx context.Context) {
					if txt := oc.HandoverText(time.Now()); txt != "" {
						log.Printf("oncall handover: %s", txt)
						rt.Handle(ctx, channel.Message{
							Channel: cfg.Oncall.Channel, ChatID: cfg.Oncall.ChatID,
							ChatType: "group", MentionedMe: true,
							UserName: "oncall", Text: txt,
						})
					}
				},
			})
		}
	}

	// ---- doc watchers ----
	if len(cfg.DocWatch) > 0 {
		byName := map[string]config.DocWatchConfig{}
		for _, e := range cfg.DocWatch {
			byName[e.Name] = e
			log.Printf("docwatch %s @ %s (every %ds) → %s:%s", e.Name, e.Path, e.IntervalSec, e.Channel, e.ChatID)
		}
		docwatch.New(cfg.DocWatch, func(name, prompt string) {
			e := byName[name]
			rt.Handle(context.Background(), channel.Message{
				Channel: e.Channel, ChatID: e.ChatID, ThreadID: e.ThreadID,
				ChatType: "p2p", MentionedMe: true,
				UserName: "docwatch:" + name, Text: prompt,
			})
		}).Start(ctx)
	}

	// ---- admin endpoints ----
	started := time.Now()
	mountAdmin(&mux, cfg, rt, chans, stop, started)
	mountAsk(&mux, cfg, chans, askBroker)
	mountWebChat(&mux, cfg, webCh)

	// ---- dashboard ----
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})

	inbound := b.Inbound()
	for _, ch := range chans {
		c := ch
		go func() {
			backoff := minChannelBackoff
			for {
				started := time.Now()
				err := c.Start(ctx, inbound)
				ran := time.Since(started)
				if err == nil || ran >= healthyChannelRun {
					// clean stop, or it ran long enough to count as healthy
					backoff = minChannelBackoff
				}
				if err != nil && ctx.Err() == nil {
					log.Printf("channel %s exited after %s: %v — restarting in %s",
						c.Name(), ran.Round(time.Second), err, backoff)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if err != nil {
					backoff = nextBackoff(backoff)
				}
			}
		}()
	}

	srv := &http.Server{Addr: cfg.HTTP.Listen, Handler: &mux}
	go func() {
		log.Printf("http listening on %s", cfg.HTTP.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	// drain in-flight turns (bounded): their usage records and session
	// updates only land when the turn goroutine finishes
	for deadline := time.Now().Add(8 * time.Second); rt.Busy() > 0 && time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
	}
	rt.Shutdown()
	ul.Close()
	return 0
}

func mountAdmin(mux *http.ServeMux, cfg *config.Config, rt *agent.Runtime, chans map[string]channel.Channel, stop func(), started time.Time) {
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if cfg.HTTP.AdminToken == "" {
			return true
		}
		if r.Header.Get("X-Admin-Token") == cfg.HTTP.AdminToken ||
			r.URL.Query().Get("token") == cfg.HTTP.AdminToken {
			return true
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized"))
		return false
	}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("/admin/status", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		chState := map[string]bool{}
		for name, ch := range chans {
			chState[name] = ch.Healthy()
		}
		writeJSON(w, map[string]any{
			"channels": chState,
			"runtime":  rt.Status(),
			"agents":   len(cfg.Agents),
			"cron":     len(cfg.Cron),
			"version":  version.Version,
		})
	})
	mux.HandleFunc("/admin/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !guard(w, r) {
			return
		}
		writeJSON(w, map[string]string{"status": "stopping"})
		go func() {
			time.Sleep(200 * time.Millisecond)
			stop()
		}()
	})
	mux.HandleFunc("/admin/usage", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		aggs, err := usage.Summarize(cfg.DataDir)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		writeJSON(w, aggs)
	})
	mux.HandleFunc("/admin/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !guard(w, r) {
			return
		}
		limit := 20
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 200 {
			limit = n
		}
		msgs := rt.Sessions().Snapshot(session.Key{
			Channel:  r.URL.Query().Get("channel"),
			ChatID:   r.URL.Query().Get("chatId"),
			ThreadID: r.URL.Query().Get("threadId"),
		})
		if len(msgs) > limit {
			msgs = msgs[len(msgs)-limit:]
		}
		writeJSON(w, map[string]any{"messages": msgs, "count": len(msgs)})
	})
	mux.HandleFunc("/admin/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !guard(w, r) {
			return
		}
		var req struct {
			Channel  string `json:"channel"`
			ChatID   string `json:"chatId"`
			ThreadID string `json:"threadId"`
			Text     string `json:"text"`
			File     string `json:"file"` // optional local path on the chatmux host
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("bad json: " + err.Error()))
			return
		}
		ch, ok := chans[req.Channel]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("unknown channel"))
			return
		}
		if req.File != "" {
			fs, ferr := ch.SenderFor(channel.Message{ChatID: req.ChatID, ThreadID: req.ThreadID})
			if ferr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(ferr.Error()))
				return
			}
			uf, ok := fs.(channel.FileSender)
			if !ok {
				w.WriteHeader(http.StatusNotImplemented)
				_, _ = w.Write([]byte("channel does not support file upload"))
				return
			}
			id, err := uf.SendFile(r.Context(), req.File, req.Text)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(err.Error()))
				return
			}
			writeJSON(w, map[string]string{"fileId": id})
			return
		}
		sender, err := ch.SenderFor(channel.Message{ChatID: req.ChatID, ThreadID: req.ThreadID})
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		id, err := sender.Send(r.Context(), req.Text, channel.SendOptions{Final: true})
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		writeJSON(w, map[string]string{"messageId": id})
	})

	// Prometheus scrape endpoint (no token: bind localhost or firewall it).
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		gauge := func(name, help string, v float64) {
			b.WriteString("# HELP " + name + " " + help + "\n# TYPE " + name + " gauge\n" +
				fmt.Sprintf("%s %v\n", name, v))
		}
		st := rt.Status()
		b.WriteString("# HELP chatmux_channels_healthy Whether each channel transport is healthy (1/0).\n# TYPE chatmux_channels_healthy gauge\n")
		for _, n := range sortedChannelNames(chans) {
			h := 0
			if chans[n].Healthy() {
				h = 1
			}
			fmt.Fprintf(&b, "chatmux_channels_healthy{channel=%q} %d\n", n, h)
		}
		gauge("chatmux_sessions_active", "Conversation anchors held in the session store.", toF(st["sessions"]))
		gauge("chatmux_turns_in_flight", "Foreground turns currently running.", toF(st["turnsInFlight"]))
		gauge("chatmux_bg_jobs_active", "Background jobs currently running.", float64(len(toI(st["backgroundJobs"]))))
		gauge("chatmux_uptime_seconds", "Seconds since server start.", time.Since(started).Seconds())
		wdFires, sRetries := metrics.Snapshot()
		b.WriteString("# HELP chatmux_turn_watchdog_fires_total Turns cancelled by the turn-budget watchdog.\n# TYPE chatmux_turn_watchdog_fires_total counter\n" +
			fmt.Sprintf("chatmux_turn_watchdog_fires_total %d\n", wdFires))
		b.WriteString("# HELP chatmux_stream_retries_total Model stream starts retried after transient failure.\n# TYPE chatmux_stream_retries_total counter\n" +
			fmt.Sprintf("chatmux_stream_retries_total %d\n", sRetries))

		aggs, err := usage.Summarize(cfg.DataDir)
		if err == nil {
			b.WriteString("# HELP chatmux_usage_calls_total Model/CLI calls per agent.\n# TYPE chatmux_usage_calls_total counter\n")
			for _, a := range aggs {
				if a.Agent == "feedback" {
					continue
				}
				fmt.Fprintf(&b, "chatmux_usage_calls_total{agent=%q} %d\n", a.Agent, a.Calls)
			}
			b.WriteString("# HELP chatmux_usage_errors_total Failed calls per agent.\n# TYPE chatmux_usage_errors_total counter\n")
			for _, a := range aggs {
				if a.Agent == "feedback" {
					continue
				}
				fmt.Fprintf(&b, "chatmux_usage_errors_total{agent=%q} %d\n", a.Agent, a.Errors)
			}
			b.WriteString("# HELP chatmux_usage_input_tokens_total Input tokens per agent.\n# TYPE chatmux_usage_input_tokens_total counter\n")
			for _, a := range aggs {
				if a.Agent == "feedback" {
					continue
				}
				fmt.Fprintf(&b, "chatmux_usage_input_tokens_total{agent=%q} %d\n", a.Agent, a.InputTokens)
			}
			b.WriteString("# HELP chatmux_usage_output_tokens_total Output tokens per agent.\n# TYPE chatmux_usage_output_tokens_total counter\n")
			for _, a := range aggs {
				if a.Agent == "feedback" {
					continue
				}
				fmt.Fprintf(&b, "chatmux_usage_output_tokens_total{agent=%q} %d\n", a.Agent, a.OutputTokens)
			}
			for _, a := range aggs {
				if a.Agent == "feedback" {
					fmt.Fprintf(&b, "# HELP chatmux_feedback_up_total Positive feedback presses.\n# TYPE chatmux_feedback_up_total counter\nchatmux_feedback_up_total %d\n", a.FeedbackUp)
					fmt.Fprintf(&b, "# HELP chatmux_feedback_down_total Negative feedback presses.\n# TYPE chatmux_feedback_down_total counter\nchatmux_feedback_down_total %d\n", a.FeedbackDown)
				}
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}

func sortedChannelNames(chans map[string]channel.Channel) []string {
	names := make([]string, 0, len(chans))
	for n := range chans {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func toF(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

func toI(v any) []any {
	x, _ := v.([]any)
	return x
}

// formatFormSubmit renders a submitted form card as the synthetic user
// message the agent will see (fields ordered per the config definition).
func formatFormSubmit(cfg *config.Config, form, operator string, values map[string]string) string {
	en := cfg.I18n != nil && cfg.I18n.Locale == "en"
	var b strings.Builder
	if en {
		fmt.Fprintf(&b, "[form %s] submitted by %s:\n", form, operator)
	} else {
		fmt.Fprintf(&b, "【表单 %s】%s 提交：\n", form, operator)
	}
	for _, fc := range cfg.Forms {
		if fc.Name != form {
			continue
		}
		for _, f := range fc.Fields {
			if v := strings.TrimSpace(values[f.Name]); v != "" {
				fmt.Fprintf(&b, "- %s: %s\n", f.Label, v)
			}
		}
		break
	}
	// values outside the schema (form edited since config load)
	known := map[string]bool{}
	for _, fc := range cfg.Forms {
		if fc.Name != form {
			continue
		}
		for _, f := range fc.Fields {
			known[f.Name] = true
		}
	}
	for k, v := range values {
		if !known[k] && strings.TrimSpace(v) != "" {
			fmt.Fprintf(&b, "- %s: %s\n", k, v)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// mountAsk serves POST /admin/ask: a blocking question into a chat —
// feishu gets an interactive form card, other channels get plain text
// (the next valid reply in that chat answers it). The HTTP request
// parks until answered or timed out; client `chatmux ask` wraps it for
// CLI agents.
func mountAsk(mux *http.ServeMux, cfg *config.Config, chans map[string]channel.Channel, broker *ask.Broker) {
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if cfg.HTTP.AdminToken == "" {
			return true
		}
		if r.Header.Get("X-Admin-Token") == cfg.HTTP.AdminToken ||
			r.URL.Query().Get("token") == cfg.HTTP.AdminToken {
			return true
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized"))
		return false
	}
	mux.HandleFunc("/admin/ask", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !guard(w, r) {
			return
		}
		if r.Method == http.MethodGet { // outstanding questions (dashboard)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.Pending())
			return
		}
		var req struct {
			Channel    string   `json:"channel"`
			ChatID     string   `json:"chatId"`
			ThreadID   string   `json:"threadId,omitempty"`
			Question   string   `json:"question"`
			Options    []string `json:"options,omitempty"`
			TimeoutSec int      `json:"timeoutSec,omitempty"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("bad json: " + err.Error()))
			return
		}
		ch, ok := chans[req.Channel]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("unknown channel"))
			return
		}
		if strings.TrimSpace(req.Question) == "" || req.ChatID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("question and chatId are required"))
			return
		}
		sender, err := ch.SenderFor(channel.Message{ChatID: req.ChatID, ThreadID: req.ThreadID})
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		p := broker.Register(req.Channel, req.ChatID, req.Options)

		delivered := false
		if fs, ok := sender.(channel.FormSender); ok {
			field := channel.FormField{Name: "answer", Label: req.Question, Type: "input", Required: boolPtr(true)}
			if len(req.Options) > 0 {
				field.Type, field.Options = "select", req.Options
			}
			delivered = fs.SendForm(r.Context(), "❓ "+req.Question, "chatmux_ask:"+p.ID(), "", []channel.FormField{field}) == nil
		}
		if !delivered {
			text := "❓ " + req.Question
			if len(req.Options) > 0 {
				text += "\n" + strings.Join(req.Options, " / ")
			}
			_, _ = sender.Send(r.Context(), text+"\n（直接回复本条作答）", channel.SendOptions{Final: true})
		}
		answer, err := p.Wait(r.Context(), time.Duration(req.TimeoutSec)*time.Second)
		if err != nil {
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte("ask timed out or cancelled: " + err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"answer": answer})
	})
}

func boolPtr(b bool) *bool { return &b }

// mountWebChat serves the dashboard chat console: POST a line in,
// GET the log (long-poll friendly cursor via ?since=).
func mountWebChat(mux *http.ServeMux, cfg *config.Config, wc *web.Web) {
	guard := func(r *http.Request) bool {
		if cfg.HTTP.AdminToken == "" {
			return true
		}
		return r.Header.Get("X-Admin-Token") == cfg.HTTP.AdminToken ||
			r.URL.Query().Get("token") == cfg.HTTP.AdminToken
	}
	mux.HandleFunc("/admin/chat", func(w http.ResponseWriter, r *http.Request) {
		if !guard(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("unauthorized"))
			return
		}
		switch r.Method {
		case http.MethodPost:
			var req struct {
				Text string `json:"text"`
				User string `json:"user,omitempty"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("bad json: " + err.Error()))
				return
			}
			if strings.TrimSpace(req.Text) == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("text is required"))
				return
			}
			user := req.User
			if user == "" {
				user = "console"
			}
			wc.Inject("console", "web:"+user, user, strings.TrimSpace(req.Text))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
		case http.MethodGet:
			since := 0
			if n, err := strconv.Atoi(r.URL.Query().Get("since")); err == nil && n >= 0 {
				since = n
			}
			entries, cursor := wc.Log(since)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries, "cursor": cursor})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// loadSkills reads skill prompts from dir into name→body (+ name→desc):
// flat `*.md` files and Claude-style `<name>/SKILL.md` directories both
// work; optional `---\ndescription: …\n---` frontmatter supplies the
// /skills listing line (and is stripped from the injected body).
func loadSkills(dir string) (map[string]string, map[string]string) {
	if dir == "" {
		return nil, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	m := map[string]string{}
	descs := map[string]string{}
	add := func(name, path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		body, desc := stripFrontmatter(string(b))
		m[name] = body
		if desc != "" {
			descs[name] = desc
		}
	}
	for _, e := range ents {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err == nil {
				add(e.Name(), filepath.Join(dir, e.Name(), "SKILL.md"))
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		add(strings.TrimSuffix(e.Name(), ".md"), filepath.Join(dir, e.Name()))
	}
	if len(m) > 0 {
		log.Printf("loaded %d skills from %s", len(m), dir)
	}
	return m, descs
}

// stripFrontmatter pulls a leading `---` YAML block's description out
// and returns the remaining body.
func stripFrontmatter(s string) (body, desc string) {
	if !strings.HasPrefix(s, "---\n") {
		return s, ""
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return s, ""
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		if v, ok := strings.CutPrefix(line, "description:"); ok {
			desc = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	return strings.TrimLeft(rest[end+4:], "\r\n"), desc
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>chatmux 看板</title>
<style>
:root { color-scheme: light dark; }
body { font-family: -apple-system, "PingFang SC", sans-serif; margin: 2rem auto; max-width: 880px; padding: 0 1rem; }
h1 { font-size: 1.4rem; } h2 { font-size: 1.1rem; margin-top: 2rem; }
table { border-collapse: collapse; width: 100%; margin-top: .5rem; }
td, th { border: 1px solid color-mix(in srgb, currentColor 20%, transparent); padding: .4rem .6rem; text-align: left; font-size: .9rem; }
.good { color: #2e9e5b; font-weight: 600; } .bad { color: #d05050; font-weight: 600; }
#token { width: 14rem; padding: .25rem; }
.muted { opacity: .6; font-size: .85rem; }
</style>
</head>
<body>
<h1>chatmux 看板</h1>
<p class="muted">token: <input id="token" placeholder="adminToken（可空）"> <button onclick="saveToken()">保存</button>
 · <span id="ts"></span></p>
<h2>通道</h2><table id="channels"><tr><td colspan="2" class="muted">加载中…</td></tr></table>
<h2>运行时</h2><table id="runtime"><tr><td colspan="2" class="muted">加载中…</td></tr></table>
<h2>会话板</h2><table id="sessions"><tr><td colspan="5" class="muted">加载中…</td></tr></table>
<h2>需要你回答</h2><table id="asks"><tr><td class="muted">无</td></tr></table>
<h2>后台任务</h2><table id="jobs"><tr><td class="muted">无</td></tr></table>
<h2>用量</h2><table id="usage"><tr><td colspan="7" class="muted">加载中…</td></tr></table>
<h2>会话终端</h2>
<div id="chatlog" style="border:1px solid color-mix(in srgb, currentColor 25%, transparent); border-radius:.5rem; padding:.6rem; height:14rem; overflow-y:auto; font-size:.9rem; margin-top:.5rem; white-space:pre-wrap;"></div>
<p style="display:flex; gap:.5rem; margin-top:.5rem;">
<input id="chatin" placeholder="对网关说话（回车发送）" style="flex:1; padding:.4rem;" onkeydown="if(event.key==='Enter')sendChat()">
<button onclick="sendChat()">发送</button>
</p>
<script>
const $ = id => document.getElementById(id);
let token = localStorage.getItem("chatmuxAdminToken") || "";
$("token").value = token;
function saveToken(){ token = $("token").value; localStorage.setItem("chatmuxAdminToken", token); refresh(); }
async function get(path){
  const h = token ? {"X-Admin-Token": token} : {};
  const r = await fetch(path, {headers: h});
  if(!r.ok) throw new Error(path + " → " + r.status);
  return r.json();
}
function esc(s){ const d=document.createElement("div"); d.textContent=String(s); return d.innerHTML; }
async function refresh(){
  try {
    const st = await get("/admin/status");
    let c = "<tr><th>通道</th><th>状态</th></tr>";
    for(const [n,ok] of Object.entries(st.channels))
      c += "<tr><td>"+esc(n)+"</td><td class='"+(ok?"good":"bad")+"'>"+(ok?"正常":"异常")+"</td></tr>";
    $("channels").innerHTML = c;
    let r = "<tr><th>指标</th><th>值</th></tr>";
    for(const [k,v] of Object.entries(st.runtime)){
      if(k==="backgroundJobs"||k==="sessionList") continue;
      r += "<tr><td>"+esc(k)+"</td><td>"+esc(Array.isArray(v)?v.join(", "):v)+"</td></tr>";
    }
    $("runtime").innerHTML = r;
    let ss = "<tr><th>会话</th><th>名称</th><th>agent</th><th>消息</th><th>状态</th></tr>";
    (st.runtime.sessionList||[]).forEach(x=>{
      ss += "<tr><td>"+esc(x.anchor)+"</td><td>"+esc(x.title||"-")+"</td><td>"+esc(x.agent||x.model||"-")+"</td><td>"+x.messages+"</td><td class='"+(x.busy?"bad":"good")+"'>"+(x.busy?"处理中":"空闲")+"</td></tr>";
    });
    $("sessions").innerHTML = (st.runtime.sessionList||[]).length ? ss : "<tr><td colspan='5' class='muted'>暂无会话</td></tr>";
    try {
      const asks = await get("/admin/ask");
      let a = "<tr><th>提问</th><th>聊天</th><th>等待</th></tr>";
      (asks||[]).forEach(x=>{
        a += "<tr><td>"+esc(x.id)+"</td><td>"+esc(x.channel+":"+x.chatId)+"</td><td>"+esc(x.since||"")+"</td></tr>";
      });
      $("asks").innerHTML = (asks||[]).length ? a : "<tr><td class='muted'>无</td></tr>";
    } catch(e){ $("asks").innerHTML = "<tr><td class='muted'>"+esc(e.message)+"</td></tr>"; }
    let j = "<tr><th>ID</th><th>类型</th><th>描述</th><th>已跑</th></tr>";
    (st.runtime.backgroundJobs||[]).forEach(x=>{
      j += "<tr><td>"+esc(x.id)+"</td><td>"+esc(x.flow||"任务")+"</td><td>"+esc(x.desc)+"</td><td>"+x.elapsedSec+"s</td></tr>";
    });
    $("jobs").innerHTML = (st.runtime.backgroundJobs||[]).length ? j : "<tr><td class='muted'>无</td></tr>";
  } catch(e){ $("channels").innerHTML = "<tr><td class='bad'>"+esc(e.message)+"（检查 token）</td></tr>"; }
  try {
    const us = await get("/admin/usage");
    let u = "<tr><th>Agent</th><th>调用</th><th>失败</th><th>均耗时</th><th>入tokens</th><th>出tokens</th><th>👍</th><th>👎</th></tr>";
    const dur = ms => ms<1000 ? ms+"ms" : ms<60000 ? (ms/1000).toFixed(1)+"s" : Math.floor(ms/60000)+"m"+Math.floor(ms%60000/1000)+"s";
    (us||[]).forEach(a=>{ u += "<tr><td>"+esc(a.agent)+"</td><td>"+a.calls+"</td><td>"+(a.errors||0)+"</td><td class='muted'>"+(a.avgMs ? "⏱"+dur(a.avgMs) : "—")+"</td><td>"+a.inputTokens+"</td><td>"+a.outputTokens+"</td><td>"+a.feedbackUp+"</td><td>"+a.feedbackDown+"</td></tr>"; });
    $("usage").innerHTML = (us||[]).length ? u : "<tr><td colspan='8' class='muted'>暂无数据</td></tr>";
  } catch(e){ $("usage").innerHTML = "<tr><td colspan='8' class='muted'>"+esc(e.message)+"</td></tr>"; }
  $("ts").textContent = "更新于 " + new Date().toLocaleTimeString();
}
refresh();
setInterval(refresh, 5000);
let chatCursor = 0;
function renderChat(entries){
  const log = $("chatlog");
  entries.forEach(e=>{
    const line = document.createElement("div");
    line.style.marginBottom = ".35rem";
    const who = e.from==="you" ? "🧑 你" : (e.from==="bot" ? "🤖" : "· "+e.from);
    line.textContent = who+"： "+e.text;
    if(e.from!=="you") line.style.opacity = "0.9";
    log.appendChild(line);
  });
  if(entries.length) log.scrollTop = log.scrollHeight;
}
async function pollChat(){
  try {
    const r = await get("/admin/chat?since="+chatCursor);
    chatCursor = r.cursor;
    renderChat(r.entries||[]);
  } catch(e){}
}
async function sendChat(){
  const inp = $("chatin");
  const text = inp.value.trim();
  if(!text) return;
  inp.value = "";
  const h = token ? {"X-Admin-Token": token, "Content-Type":"application/json"} : {"Content-Type":"application/json"};
  const r = await fetch("/admin/chat", {method:"POST", headers:h, body:JSON.stringify({text})});
  if(!r.ok){ renderChat([{from:"system", text:"发送失败 "+r.status}]); return; }
  pollChat();
}
pollChat();
setInterval(pollChat, 2000);
</script>
</body>
</html>
`
