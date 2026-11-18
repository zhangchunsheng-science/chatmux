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
	"strings"
	"syscall"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/agent"
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
	if cmd != "status" && cmd != "stop" && cmd != "send" {
		fmt.Fprintf(os.Stderr, "unknown command %q (want status|stop|send)\n", cmd)
		return 2
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
		fs.Parse(args[1:])
		text := strings.Join(fs.Args(), " ")
		if *to == "" || text == "" {
			fmt.Fprintln(os.Stderr, `usage: chatmux send -to channel:chatId[:threadId] 文本`)
			return 2
		}
		parts := strings.SplitN(*to, ":", 3)
		payload := map[string]string{"channel": parts[0], "chatId": parts[1], "text": text}
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
	rt.SetUsageLogger(usage.New(cfg.DataDir))
	in := b.Subscribe()
	go func() {
		for msg := range in {
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
	mountAdmin(&mux, cfg, rt, chans, stop)

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

func mountAdmin(mux *http.ServeMux, cfg *config.Config, rt *agent.Runtime, chans map[string]channel.Channel, stop func()) {
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
}
