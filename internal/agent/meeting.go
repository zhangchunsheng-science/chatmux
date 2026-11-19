package agent

import (
	"context"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/usage"
)

// handleMeetingEvent reacts to VC meeting lifecycle events (feishu VC
// subscription): usage-trace always, announce to cfg.Meeting's chat when
// configured. No model turn — the platform gives us facts, not dialogue.
func (r *Runtime) handleMeetingEvent(msg channel.Message) {
	if r.usage != nil {
		r.usage.Log(usage.Record{Agent: "event", Mode: "meeting:" + msg.Event, Channel: msg.Channel, ChatID: msg.ChatID})
	}
	mc := r.cfg.Meeting
	if mc == nil || mc.Channel == "" || mc.ChatID == "" || msg.Text == "" {
		return
	}
	sender, err := r.senderFor(channel.Message{Channel: mc.Channel, ChatID: mc.ChatID, ChatType: "group"})
	if err != nil {
		return
	}
	_, _ = sender.Send(context.Background(), msg.Text, channel.SendOptions{Final: true})
}
