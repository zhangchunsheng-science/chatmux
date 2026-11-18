// chatmux: one binary, many chat platforms, one brain.
//
// Bridges IM channels (Feishu, DingTalk, WeCom, Telegram, Discord,
// Slack, ...) to AI model providers (OpenAI-compatible, Anthropic) with
// streaming replies, per-thread sessions and durable dedup.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/agent"
	"github.com/zhangchunsheng-science/chatmux/internal/bus"
	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/discord"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/feishu"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/dingtalk"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/slack"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/telegram"
	"github.com/zhangchunsheng-science/chatmux/internal/channel/wecom"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/provider"
)

func main() {
	cfgPath := flag.String("config", "chatmux.json", "path to chatmux.json")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
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
		chans[ch.Name()] = ch
		if m, ok := ch.(channel.HTTPMounter); ok {
			for path, h := range m.Endpoints() {
				mux.Handle(path, h)
			}
		}
	}

	// ---- runtime ----
	b := bus.New()
	rt := agent.NewRuntime(cfg, registry, chans)
	in := b.Subscribe()
	go func() {
		for msg := range in {
			go rt.Handle(context.Background(), msg)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
}
