// /oncall: show the duty rotation from config (current member, shift
// end, order, upcoming) — chatmux's take on botmux oncall.
package agent

import (
	"context"
	"strings"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/oncall"
	"github.com/zhangchunsheng-science/chatmux/internal/usage"
)

// handleMemberEvent reacts to bot_added/bot_removed membership events:
// welcome on join, quiet log on leave (botmux group suite's greeting).
func (r *Runtime) handleMemberEvent(msg channel.Message) {
	sender, err := r.senderFor(msg)
	if err != nil {
		return
	}
	if msg.Event == "bot_added" {
		text := firstNonEmpty(r.cfg.GroupWelcome, r.say("group.welcome"))
		_, _ = sender.Send(context.Background(), text, channel.SendOptions{Final: true})
	}
	// bot_removed: nothing to send (we're out of the chat) — usage trace only
	if r.usage != nil {
		r.usage.Log(usage.Record{Agent: "event", Mode: "group:" + msg.Event, Channel: msg.Channel, ChatID: msg.ChatID})
	}
}

// handleOncallCommand renders /oncall [/next].
func (r *Runtime) handleOncallCommand(reply func(string), sub string) {
	oc := r.cfg.Oncall
	if oc == nil || len(oc.Members) == 0 {
		reply(r.say("oncall.none"))
		return
	}
	s, err := oncall.Parse(*oc, time.Now())
	if err != nil {
		reply(err.Error())
		return
	}
	now := time.Now()
	if sub == "next" {
		lines := []string{r.say("oncall.next.header")}
		up := s.Upcoming(now, 3)
		period := 1
		if s.Weekly {
			period = 7
		}
		from := s.ShiftEnd(now)
		for _, m := range up {
			to := from.AddDate(0, 0, period)
			lines = append(lines, r.say("oncall.next.line", m, from.Format("01-02"), to.Format("01-02")))
			from = to
		}
		reply(strings.Join(lines, "\n"))
		return
	}
	reply(r.say("oncall.now", s.At(now), s.ShiftEnd(now).Format("01-02 15:04"), s.Roster()))
}
