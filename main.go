// chatmux: one binary, many chat platforms, one brain.
//
// Bridges IM channels (Feishu, DingTalk, WeCom, Telegram, Discord,
// Slack, ...) to AI model providers (OpenAI-compatible, Anthropic) or
// headless CLI agents (claude/codex), with streaming replies, per-thread
// sessions, durable dedup, cron schedules and durable usage accounting.
//
// Server:   chatmux -config chatmux.json
// Client:   chatmux status|stop|send [-config chatmux.json]
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
	"sort"
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
	"github.com/zhangchunsheng-science/chatmux/internal/channel/wecom"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/cron"
	"github.com/zhangchunsheng-science/chatmux/internal/provider"
	"github.com/zhangchunsheng-science/chatmux/internal/usage"
)

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
	if cmd != "status" && cmd != "stop" && cmd != "send" && cmd != "ask" {
		fmt.Fprintf(os.Stderr, "unknown command %q (want status|stop|send|ask)\n", cmd)
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
	}
	return 0
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
	factories := map[string]func(opts json.RawMessage) (channel.Channel, error){
		"feishu":   feishu.New,
		"dingtalk": dingtalk.New,
		"wecom":    wecom.New,
		"telegram": telegram.New,
		"discord":  discord.New,
		"slack":    slack.New,
	}
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

	// ---- runtime ----
	b := bus.New()
	rt := agent.NewRuntime(cfg, registry, chans)
	ul := usage.New(cfg.DataDir)
	rt.SetUsageLogger(ul)
	rt.SetSkills(loadSkills(cfg.SkillsDir))
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

	// ---- admin endpoints ----
	started := time.Now()
	mountAdmin(&mux, cfg, rt, chans, stop, started)
	mountAsk(&mux, cfg, chans, askBroker)

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
			for {
				if err := c.Start(ctx, inbound); err != nil {
					log.Printf("channel %s exited: %v — restarting in 5s", c.Name(), err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
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

		aggs, err := usage.Summarize(cfg.DataDir)
		if err == nil {
			b.WriteString("# HELP chatmux_usage_calls_total Model/CLI calls per agent.\n# TYPE chatmux_usage_calls_total counter\n")
			for _, a := range aggs {
				if a.Agent == "feedback" {
					continue
				}
				fmt.Fprintf(&b, "chatmux_usage_calls_total{agent=%q} %d\n", a.Agent, a.Calls)
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
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !guard(w, r) {
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

// loadSkills reads *.md prompts from dir into name→body map.
func loadSkills(dir string) map[string]string {
	if dir == "" {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		m[name] = string(b)
	}
	if len(m) > 0 {
		log.Printf("loaded %d skills from %s", len(m), dir)
	}
	return m
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
<h2>后台任务</h2><table id="jobs"><tr><td class="muted">无</td></tr></table>
<h2>用量</h2><table id="usage"><tr><td colspan="6" class="muted">加载中…</td></tr></table>
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
      if(k==="backgroundJobs") continue;
      r += "<tr><td>"+esc(k)+"</td><td>"+esc(Array.isArray(v)?v.join(", "):v)+"</td></tr>";
    }
    $("runtime").innerHTML = r;
    let j = "<tr><th>ID</th><th>类型</th><th>描述</th><th>已跑</th></tr>";
    (st.runtime.backgroundJobs||[]).forEach(x=>{
      j += "<tr><td>"+esc(x.id)+"</td><td>"+esc(x.flow||"任务")+"</td><td>"+esc(x.desc)+"</td><td>"+x.elapsedSec+"s</td></tr>";
    });
    $("jobs").innerHTML = (st.runtime.backgroundJobs||[]).length ? j : "<tr><td class='muted'>无</td></tr>";
  } catch(e){ $("channels").innerHTML = "<tr><td class='bad'>"+esc(e.message)+"（检查 token）</td></tr>"; }
  try {
    const us = await get("/admin/usage");
    let u = "<tr><th>Agent</th><th>调用</th><th>入tokens</th><th>出tokens</th><th>👍</th><th>👎</th></tr>";
    (us||[]).forEach(a=>{ u += "<tr><td>"+esc(a.agent)+"</td><td>"+a.calls+"</td><td>"+a.inputTokens+"</td><td>"+a.outputTokens+"</td><td>"+a.feedbackUp+"</td><td>"+a.feedbackDown+"</td></tr>"; });
    $("usage").innerHTML = (us||[]).length ? u : "<tr><td colspan='6' class='muted'>暂无数据</td></tr>";
  } catch(e){ $("usage").innerHTML = "<tr><td colspan='6' class='muted'>"+esc(e.message)+"</td></tr>"; }
  $("ts").textContent = "更新于 " + new Date().toLocaleTimeString();
}
refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>
`
