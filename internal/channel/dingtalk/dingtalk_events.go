package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/event"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

// Classic 群会话事件 (event subscription in the DingTalk console):
// the app receives them for chats the robot is in — including its own
// addition, where the robot's id appears in the member list.
const (
	evtChatAddMember    = "chat_add_member"
	evtChatRemoveMember = "chat_remove_member"
	evtChatDisband      = "chat_disband"
)

// groupChangePayload covers the documented shapes of the three events;
// unknown fields are ignored, both id-list spellings are accepted.
type groupChangePayload struct {
	ConversationID string   `json:"conversationId"`
	ChatID         string   `json:"chatId"`
	AddUserIds     []string `json:"addUserIds"`
	AddStaffIds    []string `json:"addStaffIdList"`
	DelUserIds     []string `json:"delUserIds"`
	DelStaffIds    []string `json:"delStaffIdList"`
	Operator       string   `json:"operatorStaffId"`
}

// ProactiveTarget is the Raw of synthetic member-event messages: it tells
// SenderFor to reply through the proactive group-send API instead of a
// per-message sessionWebhook (which events do not carry).
type ProactiveTarget struct {
	ConversationID string
	RobotCode      string
}

// handleEvent maps one subscribed DingTalk event to a synthetic channel
// message. ok=false means: not a group event / duplicate / no chat id.
func (d *DingTalk) handleEvent(h *event.EventHeader, data []byte) (channel.Message, bool) {
	if h == nil {
		return channel.Message{}, false
	}
	var kind string
	switch h.EventType {
	case evtChatAddMember, evtChatRemoveMember, evtChatDisband:
		kind = h.EventType
	default:
		return channel.Message{}, false
	}
	if !d.seen.Claim("evt:" + h.EventId) { // stream mode redelivers until acked
		return channel.Message{}, false
	}
	var p groupChangePayload
	if len(data) > 0 {
		_ = json.Unmarshal(data, &p)
	}
	chatID := firstNonEmpty(p.ConversationID, p.ChatID)
	if chatID == "" {
		return channel.Message{}, false
	}
	ev := "group_disbanded"
	switch kind {
	case evtChatAddMember:
		ev = "member_added"
		if d.matchesBot(p.AddUserIds, p.AddStaffIds) {
			ev = "bot_added"
		}
	case evtChatRemoveMember:
		ev = "member_removed"
		if d.matchesBot(p.DelUserIds, p.DelStaffIds) {
			ev = "bot_removed"
		}
	}
	rc := firstNonEmpty(d.opts.RobotCode, d.opts.BotUserID)
	return channel.Message{
		ID:        "evt-" + h.EventId,
		Channel:   d.Name(),
		ChatID:    chatID,
		ChatType:  "group",
		UserID:    p.Operator,
		Event:     ev,
		Raw:       &ProactiveTarget{ConversationID: chatID, RobotCode: rc},
		Timestamp: time.UnixMilli(h.EventBornTime),
	}, true
}

// matchesBot reports whether the changed member list contains the robot.
// Candidates: options.botUserId first (exact userId form), then robotCode.
// With neither configured, adds/removes classify as plain member events
// (set robotCode or botUserId to enable the join welcome).
func (d *DingTalk) matchesBot(userIds, staffIds []string) bool {
	var cands []string
	if d.opts.BotUserID != "" {
		cands = append(cands, d.opts.BotUserID)
	}
	if d.opts.RobotCode != "" {
		cands = append(cands, d.opts.RobotCode)
	}
	if len(cands) == 0 {
		return false
	}
	for _, id := range append(append([]string{}, userIds...), staffIds...) {
		for _, c := range cands {
			if id == c {
				return true
			}
		}
	}
	return false
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// ---- access token for proactive sends ----

func (d *DingTalk) token(ctx context.Context) (string, error) {
	d.tokMu.Lock()
	defer d.tokMu.Unlock()
	if d.tok != "" && time.Now().Before(d.tokExp.Add(-5*time.Minute)) {
		return d.tok, nil
	}
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int64  `json:"expireIn"`
	}
	body := map[string]string{"appKey": d.opts.ClientID, "appSecret": d.opts.ClientSecret}
	if err := d.post(ctx, d.opts.APIBase+"/v1.0/oauth2/accessToken", body, &out); err != nil {
		return "", fmt.Errorf("dingtalk token: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("dingtalk token: empty accessToken")
	}
	d.tok = out.AccessToken
	d.tokExp = time.Now().Add(time.Duration(out.ExpireIn) * time.Second)
	return d.tok, nil
}

// proactiveSender sends to a group via /v1.0/robot/groupMessages/send —
// used for event-triggered messages that have no sessionWebhook.
type proactiveSender struct {
	d *DingTalk
	t *ProactiveTarget
}

func (s *proactiveSender) Send(ctx context.Context, text string, opts channel.SendOptions) (string, error) {
	if s.t.RobotCode == "" {
		return "", fmt.Errorf("dingtalk: proactive send needs robotCode (set it in channel options)")
	}
	tok, err := s.d.token(ctx)
	if err != nil {
		return "", err
	}
	msgParam, _ := json.Marshal(map[string]string{"title": "chatmux", "text": text})
	body := map[string]string{
		"msgKey":             "sampleMarkdown",
		"msgParam":           string(msgParam),
		"openConversationId": s.t.ConversationID,
		"robotCode":          s.t.RobotCode,
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", s.d.opts.APIBase+"/v1.0/robot/groupMessages/send", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("x-acs-dingtalk-access-token", tok)
	var out struct {
		MessageID string `json:"messageId"`
	}
	if err := s.d.doReq(req, &out); err != nil {
		return "", err
	}
	return out.MessageID, nil
}

func (s *proactiveSender) Replace(ctx context.Context, msgID, text string) error {
	return fmt.Errorf("dingtalk: message edit unsupported")
}

func (s *proactiveSender) React(ctx context.Context, msgID, emoji string) error {
	return fmt.Errorf("dingtalk: reactions unsupported")
}
