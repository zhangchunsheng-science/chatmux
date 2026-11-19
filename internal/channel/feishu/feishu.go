// Package feishu bridges 飞书/Lark via the official WebSocket
// long-connection (no public URL needed), or webhook callback mode.
//
// Setup: create a 企业自建应用 at open.feishu.cn, enable 机器人 capability,
// subscribe the im.message.receive_v1 event, choose 长连接 (long connection)
// event delivery, and grant im:message send/patch scopes.
package feishu

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkdispatch "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkvc "github.com/larksuite/oapi-sdk-go/v3/service/vc/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/dedup"
)

type Options struct {
	AppID     string `json:"appId"`
	AppSecret string `json:"appSecret"`
	Brand     string `json:"brand"` // "feishu" (default) or "lark"
	Mode      string `json:"mode"`  // "ws" (default) or "webhook"
	// webhook mode:
	WebhookPath       string `json:"webhookPath"` // default /feishu/events
	EncryptKey        string `json:"encryptKey"`
	VerificationToken string `json:"verificationToken"`
	// admission:
	AllowUsers []string `json:"allowUsers"` // open_ids
	AllowChats []string `json:"allowChats"` // chat_ids (groups); empty = all
	DataDir    string   `json:"dataDir"`
}

type Feishu struct {
	name         string // instance name (defaults to "feishu")
	opts         Options
	api          *api
	seen         *dedup.Store
	client       *http.Client
	feedbackHook func(kind, operator, msgID string)
	formHook     func(form, chatID, operator string, values map[string]string)

	mu        sync.Mutex
	healthy   bool
	pump      chan<- channel.Message
	botOpenID string
}

func New(raw json.RawMessage) (channel.Channel, error) {
	var o Options
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("feishu options: %w", err)
	}
	if o.AppID == "" || o.AppSecret == "" {
		return nil, fmt.Errorf("feishu: appId and appSecret are required")
	}
	if o.Mode == "" {
		o.Mode = "ws"
	}
	if o.Brand == "" {
		o.Brand = "feishu"
	}
	if o.WebhookPath == "" {
		o.WebhookPath = "/feishu/events"
	}
	if o.DataDir == "" {
		o.DataDir = "data"
	}
	base := "https://open.feishu.cn"
	if o.Brand == "lark" {
		base = "https://open.larksuite.com"
	}
	return &Feishu{
		name:   "feishu",
		opts:   o,
		api:    &api{appID: o.AppID, appSecret: o.AppSecret, base: base, client: &http.Client{Timeout: 30 * time.Second}},
		seen:   dedup.New(filepath.Join(o.DataDir, "feishu-seen.json"), 8*time.Hour, 5000),
		client: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (f *Feishu) Name() string {
	if f.name != "" {
		return f.name
	}
	return "feishu"
}

// SetName gives the instance its config name (multi-account support).
func (f *Feishu) SetName(n string) {
	f.name = n
	// per-instance dedup file: two accounts of one platform must not
	// share a seen-store (they would swallow each other's messages)
	f.seen = dedup.New(filepath.Join(f.opts.DataDir, n+"-seen.json"), 8*time.Hour, 5000)
}

func (f *Feishu) Healthy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthy
}

func (f *Feishu) setHealthy(v bool) {
	f.mu.Lock()
	f.healthy = v
	f.mu.Unlock()
}

// Start runs the transport. WS mode blocks here for the process lifetime;
// webhook mode just registers the pump (the HTTP server serves callbacks).
func (f *Feishu) Start(ctx context.Context, out chan<- channel.Message) error {
	f.mu.Lock()
	f.pump = out
	f.mu.Unlock()

	if f.opts.Mode == "webhook" {
		<-ctx.Done()
		return nil
	}

	f.botOpenID = f.api.botOpenID(ctx)

	dispatcher := larkdispatch.NewEventDispatcher(f.opts.VerificationToken, f.opts.EncryptKey).
		OnP2MessageReceiveV1(func(ctx context.Context, e *larkim.P2MessageReceiveV1) error {
			msg, ok := f.toMessage(e)
			if !ok {
				return nil
			}
			f.mu.Lock()
			pump := f.pump
			f.mu.Unlock()
			if pump != nil {
				select {
				case pump <- msg:
				case <-ctx.Done():
				case <-time.After(30 * time.Second):
				}
			}
			return nil
		}).
		OnP2CardActionTrigger(func(ctx context.Context, e *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
			return f.handleCardAction(e)
		}).
		OnP2ChatMemberBotAddedV1(func(ctx context.Context, e *larkim.P2ChatMemberBotAddedV1) error {
			if e != nil && e.Event != nil {
				f.pumpMemberEvent(ctx, e.EventV2Base, deref(e.Event.ChatId), "bot_added")
			}
			return nil
		}).
		OnP2ChatMemberBotDeletedV1(func(ctx context.Context, e *larkim.P2ChatMemberBotDeletedV1) error {
			if e != nil && e.Event != nil {
				f.pumpMemberEvent(ctx, e.EventV2Base, deref(e.Event.ChatId), "bot_removed")
			}
			return nil
		}).
		// VC meeting lifecycle (需要事件订阅 vc.bot.* / vc.meeting.*)
		OnP2BotMeetingInvitedV1(func(ctx context.Context, e *larkvc.P2BotMeetingInvitedV1) error {
			if e != nil && e.Event != nil {
				id, topic := agentMeeting(e.Event.Meeting)
				f.pumpMeetingEvent(ctx, e.EventV2Base, meetingNote{kind: "meeting_invited", id: id, topic: topic})
			}
			return nil
		}).
		OnP2BotMeetingStartedV1(func(ctx context.Context, e *larkvc.P2BotMeetingStartedV1) error {
			if e != nil && e.Event != nil {
				id, topic := agentMeeting(e.Event.Meeting)
				f.pumpMeetingEvent(ctx, e.EventV2Base, meetingNote{kind: "meeting_started", id: id, topic: topic})
			}
			return nil
		}).
		OnP2BotMeetingEndedV1(func(ctx context.Context, e *larkvc.P2BotMeetingEndedV1) error {
			if e != nil && e.Event != nil {
				id, topic := agentMeeting(e.Event.Meeting)
				f.pumpMeetingEvent(ctx, e.EventV2Base, meetingNote{kind: "meeting_ended", id: id, topic: topic})
			}
			return nil
		}).
		OnP2MeetingRecordingReadyV1(func(ctx context.Context, e *larkvc.P2MeetingRecordingReadyV1) error {
			if e != nil && e.Event != nil {
				id, topic := vcMeeting(e.Event.Meeting)
				f.pumpMeetingEvent(ctx, e.EventV2Base, meetingNote{
					kind: "meeting_recording", id: id, topic: topic,
					url: deref(e.Event.Url), duration: deref(e.Event.Duration),
				})
			}
			return nil
		})

	wsClient := larkws.NewClient(f.opts.AppID, f.opts.AppSecret,
		larkws.WithEventHandler(dispatcher),
	)
	wsClient.SetOnReady(func() { f.setHealthy(true) })
	wsClient.SetOnReconnecting(func() { f.setHealthy(false) })
	wsClient.SetOnError(func(err error) { f.setHealthy(false) })
	return wsClient.Start(ctx)
}

// pumpMemberEvent turns bot membership events (added/removed) into
// synthetic messages so the runtime can greet or log.
func (f *Feishu) pumpMemberEvent(ctx context.Context, base *larkevent.EventV2Base, chatID, kind string) {
	eventID := ""
	if base != nil && base.Header != nil {
		eventID = base.Header.EventID
	}
	if !f.seen.Claim("member|" + kind + "|" + eventID) {
		return
	}
	msg := channel.Message{
		Channel: f.name, ChatID: chatID, ChatType: "group",
		UserName: "event:" + kind, Event: kind, Timestamp: time.Now(),
	}
	f.mu.Lock()
	pump := f.pump
	f.mu.Unlock()
	if pump != nil {
		select {
		case pump <- msg:
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

// meetingNote is the distilled VC event the runtime will announce.
type meetingNote struct {
	kind      string // meeting_invited | meeting_started | meeting_ended | meeting_recording
	id, topic string
	url       string // recording link (recording_ready only)
	duration  string // msec string
}

func agentMeeting(m *larkvc.MeetingAgentEventMeeting) (id, topic string) {
	if m == nil {
		return "", ""
	}
	return deref(m.Id), deref(m.Topic)
}

func vcMeeting(m *larkvc.MeetingEventMeeting) (id, topic string) {
	if m == nil {
		return "", ""
	}
	return deref(m.Id), deref(m.Topic)
}

// formatMeeting renders the announcement text (zh, operational notice).
func formatMeeting(n meetingNote) string {
	topic := n.topic
	if topic == "" {
		topic = "未命名会议"
	}
	switch n.kind {
	case "meeting_invited":
		return "📞 机器人被邀请加入会议：" + topic + "（未配置自动入会，仅通知）"
	case "meeting_started":
		return "📞 会议开始：" + topic
	case "meeting_ended":
		return "📞 会议结束：" + topic
	case "meeting_recording":
		mins := ""
		if ms, err := strconv.Atoi(n.duration); err == nil && ms > 0 {
			mins = "（时长 " + strconv.Itoa(ms/60000) + " 分钟）"
		}
		s := "🎬 会议录制已就绪" + mins + "：" + topic
		if n.url != "" {
			s += "\n" + n.url
		}
		return s
	}
	return ""
}

// pumpMeetingEvent turns VC meeting lifecycle events into synthetic
// messages: the runtime announces them to the configured meeting chat.
func (f *Feishu) pumpMeetingEvent(ctx context.Context, base *larkevent.EventV2Base, n meetingNote) {
	eventID := ""
	if base != nil && base.Header != nil {
		eventID = base.Header.EventID
	}
	if !f.seen.Claim("meeting|" + n.kind + "|" + eventID) {
		return
	}
	msg := channel.Message{
		Channel: f.name, ChatID: n.id, ChatType: "group",
		UserName: "event:" + n.kind, Event: n.kind,
		Text: formatMeeting(n), Timestamp: time.Now(),
	}
	f.mu.Lock()
	pump := f.pump
	f.mu.Unlock()
	if pump != nil {
		select {
		case pump <- msg:
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

func (f *Feishu) toMessage(e *larkim.P2MessageReceiveV1) (channel.Message, bool) {
	if e == nil || e.Event == nil || e.Event.Message == nil {
		return channel.Message{}, false
	}
	m := e.Event.Message
	senderID, senderType := "", ""
	if e.Event.Sender != nil {
		if e.Event.Sender.SenderId != nil {
			senderID = deref(e.Event.Sender.SenderId.OpenId)
		}
		senderType = deref(e.Event.Sender.SenderType)
	}
	if senderType == "app" && !contains(f.opts.AllowUsers, senderID) {
		return channel.Message{}, false // bot-to-bot: drop unless explicitly allowed
	}
	eventID := ""
	if e.EventV2Base != nil && e.EventV2Base.Header != nil {
		eventID = e.EventV2Base.Header.EventID
	}
	if !f.seen.Claim(eventID + "|" + deref(m.MessageId)) {
		return channel.Message{}, false
	}
	chatID := deref(m.ChatId)
	if len(f.opts.AllowChats) > 0 && !contains(f.opts.AllowChats, chatID) {
		return channel.Message{}, false
	}
	if len(f.opts.AllowUsers) > 0 && !contains(f.opts.AllowUsers, senderID) {
		return channel.Message{}, false
	}

	msgType := deref(m.MessageType)
	content := deref(m.Content)
	text, attachments := parseContent(msgType, content)

	// mention handling: strip @bot placeholder text
	mentionedMe := false
	for _, mention := range m.Mentions {
		if mention == nil || mention.Id == nil {
			continue
		}
		mentionID := deref(mention.Id.OpenId)
		if mentionID == f.botOpenID || mentionID == f.opts.AppID {
			mentionedMe = true
			text = strings.TrimSpace(strings.ReplaceAll(text, "@"+deref(mention.Key), ""))
		}
	}

	chatType := deref(m.ChatType)
	thread := ""
	if deref(m.ThreadId) != "" {
		thread = deref(m.ThreadId)
	} else if deref(m.RootId) != "" {
		thread = deref(m.RootId)
	}

	msg := channel.Message{
		ID:          deref(m.MessageId),
		Channel:     f.name,
		ChatID:      chatID,
		ChatType:    chatType,
		ThreadID:    thread,
		UserID:      senderID,
		Text:        strings.TrimSpace(text),
		MentionedMe: mentionedMe,
		Attachments: attachments,
		Timestamp:   time.Now(),
	}
	// reply quote: a real reply (not a topic-group follow-up) carries
	// parent_id ≠ root; fetch the quoted text for context.
	if parent := deref(m.ParentId); parent != "" && parent != deref(m.RootId) && parent != thread {
		qctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if qText, qSender, err := f.api.getMessageText(qctx, parent); err == nil && strings.TrimSpace(qText) != "" {
			msg.Quoted = &channel.Quoted{ID: parent, UserID: qSender, Text: strings.TrimSpace(qText)}
		}
		cancel()
	}
	if msg.Text == "" && len(attachments) == 0 {
		return channel.Message{}, false
	}
	return msg, true
}

// parseContent turns the per-msg_type JSON content into plain text +
// normalized attachments.
func parseContent(msgType, content string) (string, []channel.Attachment) {
	switch msgType {
	case "text":
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return c.Text, nil
	case "post":
		// rich text: walk paragraphs, join text nodes
		var c map[string]any
		if json.Unmarshal([]byte(content), &c) != nil {
			return "", nil
		}
		return walkPost(c), nil
	case "image":
		var c struct {
			ImageKey string `json:"image_key"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return "", []channel.Attachment{{Type: "image", Name: c.ImageKey, Key: c.ImageKey}}
	case "file":
		var c struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return "", []channel.Attachment{{Type: "file", Name: c.FileName, Key: c.FileKey}}
	case "audio":
		var c struct {
			FileKey string `json:"file_key"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return "", []channel.Attachment{{Type: "audio", Name: c.FileKey, Key: c.FileKey}}
	}
	return "", nil
}

func walkPost(node any) string {
	switch v := node.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s := walkPost(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "")
	case map[string]any:
		if t, _ := v["tag"].(string); t == "text" {
			s, _ := v["text"].(string)
			return s
		}
		// structural keys: title / content / nested
		var b strings.Builder
		for _, key := range []string{"title", "content"} {
			if sub, ok := v[key]; ok {
				b.WriteString(walkPost(sub))
			}
		}
		return b.String()
	}
	return ""
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ---- webhook mode ----

// Endpoints mounts the callback receiver (webhook mode only).
func (f *Feishu) Endpoints() map[string]http.Handler {
	if f.opts.Mode != "webhook" {
		return nil
	}
	return map[string]http.Handler{f.opts.WebhookPath: http.HandlerFunc(f.serveWebhook)}
}

func (f *Feishu) serveWebhook(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	q := r.URL.Query()

	// URL verification challenge (config-time handshake)
	if strings.Contains(string(body), `"url_verification"`) || q.Get("challenge") != "" {
		var v struct {
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(body, &v)
		if v.Challenge == "" {
			v.Challenge = q.Get("challenge")
		}
		_ = json.NewEncoder(rw).Encode(map[string]string{"challenge": v.Challenge})
		return
	}

	// signature: sha256(timestamp + nonce + encryptKey + body)
	if f.opts.EncryptKey != "" {
		sig := q.Get("signature")
		ts := q.Get("timestamp")
		nonce := q.Get("nonce")
		mac := hmac.New(sha256.New, []byte(f.opts.EncryptKey))
		mac.Write([]byte(ts + nonce + f.opts.EncryptKey + string(body)))
		want := hex.EncodeToString(mac.Sum(nil))
		if sig == "" || !hmac.Equal([]byte(sig), []byte(want)) {
			http.Error(rw, "bad signature", http.StatusForbidden)
			return
		}
		// freshness: ±1h replay window
		var tsInt int64
		_, _ = fmt.Sscanf(ts, "%d", &tsInt)
		d := time.Since(time.Unix(tsInt, 0))
		if tsInt != 0 && (d > time.Hour || d < -time.Hour) {
			http.Error(rw, "stale", http.StatusForbidden)
			return
		}
	}

	_, _ = rw.Write([]byte(`{"code":0}`))

	var env struct {
		Header struct {
			EventID   string `json:"event_id"`
			Token     string `json:"token"`
			EventType string `json:"event_type"`
		} `json:"header"`
		Event json.RawMessage `json:"event"`
	}
	if json.Unmarshal(body, &env) != nil || env.Header.EventID == "" {
		return
	}
	if f.opts.VerificationToken != "" && env.Header.Token != f.opts.VerificationToken {
		return
	}
	if env.Header.EventType != "im.message.receive_v1" {
		return
	}
	if !f.seen.Claim(env.Header.EventID) {
		return
	}
	var data struct {
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
			SenderType string `json:"sender_type"`
		} `json:"sender"`
		Message struct {
			MessageID   string `json:"message_id"`
			RootID      string `json:"root_id"`
			ThreadID    string `json:"thread_id"`
			ChatID      string `json:"chat_id"`
			ChatType    string `json:"chat_type"`
			MessageType string `json:"message_type"`
			Content     string `json:"content"`
			Mentions    []struct {
				Key string `json:"key"`
				ID  string `json:"id"`
			} `json:"mentions"`
		} `json:"message"`
	}
	if json.Unmarshal(env.Event, &data) != nil {
		return
	}
	if f.botOpenID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		f.botOpenID = f.api.botOpenID(ctx)
		cancel()
	}
	text, atts := parseContent(data.Message.MessageType, data.Message.Content)
	mentionedMe := false
	for _, mn := range data.Message.Mentions {
		if mn.ID == f.botOpenID || mn.ID == f.opts.AppID {
			mentionedMe = true
			text = strings.TrimSpace(strings.ReplaceAll(text, "@"+mn.Key, ""))
		}
	}
	thread := data.Message.ThreadID
	if thread == "" {
		thread = data.Message.RootID
	}
	msg := channel.Message{
		ID:          data.Message.MessageID,
		Channel:     f.name,
		ChatID:      data.Message.ChatID,
		ChatType:    data.Message.ChatType,
		ThreadID:    thread,
		UserID:      data.Sender.SenderID.OpenID,
		Text:        strings.TrimSpace(text),
		MentionedMe: mentionedMe,
		Attachments: atts,
		Timestamp:   time.Now(),
	}
	if msg.Text == "" && len(atts) == 0 {
		return
	}
	f.mu.Lock()
	pump := f.pump
	f.mu.Unlock()
	if pump != nil {
		go func() {
			select {
			case pump <- msg:
			case <-time.After(30 * time.Second):
			}
		}()
	}
}

// ---- outbound ----

type Sender struct {
	f      *Feishu
	chatID string
	thread string
}

func (f *Feishu) SenderFor(msg channel.Message) (channel.Sender, error) {
	return &Sender{f: f, chatID: msg.ChatID, thread: msg.ThreadID}, nil
}

// Download implements channel.AttachmentDownloader: fetches image bytes
// by image_key via the im resource API (needs im:resource scope).
func (f *Feishu) Download(ctx context.Context, att channel.Attachment) ([]byte, string, error) {
	if att.Key == "" {
		return nil, "", fmt.Errorf("feishu: attachment has no key")
	}
	data, mime, err := f.api.download(ctx, "/open-apis/im/v1/images/"+att.Key)
	if err != nil {
		return nil, "", err
	}
	if mime == "" || mime == "application/octet-stream" {
		mime = http.DetectContentType(data)
	}
	return data, mime, nil
}

func (s *Sender) Send(ctx context.Context, text string, opts channel.SendOptions) (string, error) {
	if opts.Final {
		// final answers ship as cards with 👍/👎 feedback buttons;
		// mentions fold into the markdown via lark_md <at> tags
		content := feedbackCard(withMentions(text, opts.MentionUserIDs...))
		return s.f.api.sendMessage(ctx, "chat_id", s.chatID, "interactive", content, opts.ReplyTo)
	}
	// interim progress goes out as a card so Replace (PATCH) can stream it
	return s.f.api.sendMessage(ctx, "chat_id", s.chatID, "interactive", cardContent("…", text), opts.ReplyTo)
}

// Replace edits a message in place. Feishu PATCH only supports cards, so
// streaming placeholders are sent as cards; text finals never Replace.
func (s *Sender) Replace(ctx context.Context, msgID, text string) error {
	return s.f.api.patchMessage(ctx, msgID, cardContent("", text))
}

// handleCardAction answers 👍/👎 button presses and form submissions
// with a toast, fanning the event out to the installed hook.
func (f *Feishu) handleCardAction(e *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
	resp := &larkcallback.CardActionTriggerResponse{}
	if e == nil || e.Event == nil || e.Event.Action == nil {
		return resp, nil
	}
	// form submission: inputs arrive in Action.FormValue, tagged with
	// the submit button's chatmux_form field
	if fv, _ := e.Event.Action.FormValue["chatmux_form"].(string); fv != "" {
		operator, chatID := "", ""
		if e.Event.Operator != nil {
			operator = e.Event.Operator.OpenID
		}
		if e.Event.Context != nil {
			chatID = e.Event.Context.OpenChatID
		}
		values := map[string]string{}
		for k, v := range e.Event.Action.FormValue {
			if k == "chatmux_form" {
				continue
			}
			if s, ok := v.(string); ok {
				values[k] = s
			}
		}
		if f.formHook != nil {
			go f.formHook(fv, chatID, operator, values)
		}
		return &larkcallback.CardActionTriggerResponse{
			Toast: &larkcallback.Toast{Type: "success", Content: "已提交 ✓"},
		}, nil
	}
	v, _ := e.Event.Action.Value["chatmux_feedback"].(string)
	if v == "" {
		return resp, nil
	}
	operator, msgID := "", ""
	if e.Event.Operator != nil {
		operator = e.Event.Operator.OpenID
	}
	if e.Event.Context != nil {
		msgID = e.Event.Context.OpenMessageID
	}
	if f.feedbackHook != nil {
		go f.feedbackHook(v, operator, msgID)
	}
	toast := "已收到反馈 👌"
	if v == "down" {
		toast = "已收到，下次改进 💪"
	}
	return &larkcallback.CardActionTriggerResponse{
		Toast: &larkcallback.Toast{Type: "success", Content: toast},
	}, nil
}

// SetFeedbackHook installs the callback fired on 👍/👎 presses.
func (f *Feishu) SetFeedbackHook(fn func(kind, operator, msgID string)) { f.feedbackHook = fn }

// SetFormHandler installs the callback fired when a form card is
// submitted (form name, originating chat, submitting user, field values).
func (f *Feishu) SetFormHandler(fn func(form, chatID, operator string, values map[string]string)) {
	f.formHook = fn
}

// SendFile uploads a local file (image → im/v1/images, else im/v1/files)
// and posts it into the chat; a caption rides along as a text message.
func (s *Sender) SendFile(ctx context.Context, path, caption string) (string, error) {
	key, err := s.f.api.uploadFile(ctx, path, isImagePath(path))
	if err != nil {
		return "", err
	}
	msgType, content := "file", `{"file_key":`+strconv.Quote(key)+`}`
	if isImagePath(path) {
		msgType, content = "image", `{"image_key":`+strconv.Quote(key)+`}`
	}
	id, err := s.f.api.sendMessage(ctx, "chat_id", s.chatID, msgType, content, "")
	if caption != "" {
		if _, cerr := s.f.api.sendMessage(ctx, "chat_id", s.chatID, "text", textContent(caption), ""); cerr == nil && err == nil {
			return id, nil
		}
	}
	return id, err
}

func isImagePath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

// SendForm posts an interactive form card into the sender's chat.
func (s *Sender) SendForm(ctx context.Context, title, formName, desc string, fields []channel.FormField) error {
	_, err := s.f.api.sendMessage(ctx, "chat_id", s.chatID, "interactive", formCard(title, formName, desc, fields), "")
	return err
}

func (s *Sender) React(ctx context.Context, msgID, emoji string) error {
	return s.f.api.addReaction(ctx, msgID, emoji)
}
