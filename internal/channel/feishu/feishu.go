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
	"strings"
	"sync"
	"time"

	larkdispatch "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
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
func (f *Feishu) SetName(n string) { f.name = n }

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
		})

	wsClient := larkws.NewClient(f.opts.AppID, f.opts.AppSecret,
		larkws.WithEventHandler(dispatcher),
	)
	wsClient.SetOnReady(func() { f.setHealthy(true) })
	wsClient.SetOnReconnecting(func() { f.setHealthy(false) })
	wsClient.SetOnError(func(err error) { f.setHealthy(false) })
	return wsClient.Start(ctx)
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

// handleCardAction answers 👍/👎 button presses with a toast and fans
// the feedback out to the installed hook (usage log).
func (f *Feishu) handleCardAction(e *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
	resp := &larkcallback.CardActionTriggerResponse{}
	if e == nil || e.Event == nil || e.Event.Action == nil {
		return resp, nil
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

func (s *Sender) React(ctx context.Context, msgID, emoji string) error {
	return s.f.api.addReaction(ctx, msgID, emoji)
}
