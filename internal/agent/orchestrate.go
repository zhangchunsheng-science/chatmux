// /orchestrate: fan one task out to several agents in parallel, then
// (optionally) merge their answers with a synthesizer agent — chatmux's
// take on botmux's multi-bot orchestration.
//
//	/orchestrate a,b,c 任务            → replies 【a】… 【b】… 【c】…
//	/orchestrate a,b + merger 任务     → … then 【汇总 · merger】…
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
	"github.com/zhangchunsheng-science/chatmux/internal/usage"
)

func (r *Runtime) handleOrchestrate(key session.Key, msg channel.Message, sender channel.Sender) bool {
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.Text), "/orchestrate"))
	head, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	// optional merger token: "+ merger" (or ＋) right after the agent list
	mergerName := ""
	if strings.HasPrefix(rest, "+") || strings.HasPrefix(rest, "＋") {
		rest = strings.TrimSpace(strings.TrimLeft(rest, "+＋"))
		token, remainder, found := strings.Cut(rest, " ")
		if found {
			mergerName = strings.TrimSpace(token)
			rest = strings.TrimSpace(remainder)
		}
	}
	task := strings.TrimSpace(rest)
	if head == "" || task == "" {
		_, _ = sender.Send(context.Background(), r.say("orchestrate.usage"), channel.SendOptions{Final: true})
		return true
	}
	var names []string
	for _, n := range strings.Split(head, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		_, _ = sender.Send(context.Background(), r.say("orchestrate.usage"), channel.SendOptions{Final: true})
		return true
	}
	var agents []*config.AgentConfig
	for _, n := range names {
		a := r.agentByName(n)
		if a == nil {
			_, _ = sender.Send(context.Background(), r.say("orchestrate.noagent", n), channel.SendOptions{Final: true})
			return true
		}
		agents = append(agents, a)
	}
	var merger *config.AgentConfig
	if mergerName != "" {
		merger = r.agentByName(mergerName)
		if merger == nil {
			_, _ = sender.Send(context.Background(), r.say("orchestrate.noagent", mergerName), channel.SendOptions{Final: true})
			return true
		}
	}
	_, _ = sender.Send(context.Background(), r.say("orchestrate.started", len(agents), head), channel.SendOptions{Final: true})
	_ = sender.React(context.Background(), msg.ID, "rocket")

	go func() {
		results := make([]string, len(agents))
		var wg sync.WaitGroup
		for i, a := range agents {
			wg.Add(1)
			go func(i int, a *config.AgentConfig) {
				defer wg.Done()
				out, err := r.callAgentText(context.Background(), a, task)
				if err != nil {
					results[i] = r.say("orchestrate.fail", a.Name, err)
					_, _ = sender.Send(context.Background(), results[i], channel.SendOptions{Final: true})
					return
				}
				results[i] = out
				_, _ = sender.Send(context.Background(), r.say("orchestrate.part", a.Name, out), channel.SendOptions{Final: true})
			}(i, a)
		}
		wg.Wait()
		if merger != nil {
			var b strings.Builder
			b.WriteString("多个 agent 对同一任务的回答如下，请综合成一份最终答案（取长补短，注明分歧）：\n\n任务：" + task + "\n")
			for i, a := range agents {
				fmt.Fprintf(&b, "\n【%s】\n%s\n", a.Name, results[i])
			}
			merged, err := r.callAgentText(context.Background(), merger, b.String())
			if err != nil {
				_, _ = sender.Send(context.Background(), r.say("orchestrate.fail", merger.Name, err), channel.SendOptions{Final: true})
				return
			}
			_, _ = sender.Send(context.Background(), r.say("orchestrate.merged", merger.Name, merged), channel.SendOptions{Final: true})
		}
	}()
	return true
}

// callAgentText runs one stateless model call and returns the full text.
func (r *Runtime) callAgentText(ctx context.Context, a *config.AgentConfig, prompt string) (string, error) {
	prov, ok := r.registry.Get(providerName(a))
	if !ok {
		return "", fmt.Errorf("provider %s not registered", providerName(a))
	}
	start := time.Now()
	stream, err := prov.Stream(ctx, provider.Request{
		Model:       a.Model,
		System:      firstNonEmpty(a.System, defaultSystemPrompt),
		Messages:    []provider.Msg{{Role: provider.User, Content: prompt}},
		Temperature: 0.7,
		MaxTokens:   4096,
		APIKey:      a.APIKey,
		BaseURL:     a.BaseURL,
	})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	var inTok, outTok int
	for d := range stream {
		if d.Err != nil {
			return "", d.Err
		}
		sb.WriteString(d.Text)
		if d.InputTokens > 0 {
			inTok = d.InputTokens
		}
		if d.OutputTokens > 0 {
			outTok = d.OutputTokens
		}
	}
	if strings.TrimSpace(sb.String()) == "" {
		return "", fmt.Errorf("empty reply")
	}
	if r.usage != nil {
		r.usage.Log(usage.Record{
			Agent: a.Name, Mode: "orchestrate", Provider: providerName(a), Model: a.Model,
			InputTokens: inTok, OutputTokens: outTok, OutputChars: sb.Len(),
			DurationMs: time.Since(start).Milliseconds(),
		})
	}
	return sb.String(), nil
}
