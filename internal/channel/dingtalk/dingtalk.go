// Package dingtalk bridges 钉钉 robots via the official Stream Mode
// (websocket, no public URL needed).
//
// Setup: create a 企业内部应用/机器人 in the DingTalk open platform, get
// ClientID (AppKey) + ClientSecret (AppSecret), enable Stream Mode on
// the robot, subscribe message push.
package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	dingclient "github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/dedup"
)

type Options struct {
	ClientID     string   `json:"clientId"`     // AppKey
	ClientSecret string   `json:"clientSecret"` // AppSecret
	RobotCode    string   `json:"robotCode"`    // optional, defaults from message
	APIBase      string   `json:"apiBase"`      // default https://api.dingtalk.com
	DataDir      string   `json:"dataDir"`
	AllowUsers   []string `json:"allowUsers"` // staffIds
}

type DingTalk struct {
	name   string // instance name (defaults to "dingtalk")
	opts   Options
	client *http.Client
	seen   *dedup.Store

	mu      sync.Mutex
	healthy bool
}

func New(raw json.RawMessage) (channel.Channel, error) {
	var o Options
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("dingtalk options: %w", err)
	}
	if o.ClientID == "" || o.ClientSecret == "" {
		return nil, fmt.Errorf("dingtalk: clientId and clientSecret are required")
	}
	if o.APIBase == "" {
		o.APIBase = "https://api.dingtalk.com"
	}
	if o.DataDir == "" {
		o.DataDir = "data"
	}
	return &DingTalk{
		name:   "dingtalk",
		opts:   o,
		client: &http.Client{Timeout: 30 * time.Second},
		seen:   dedup.New(strings.TrimSuffix(o.DataDir, "/")+"/dingtalk-seen.json", 8*time.Hour, 5000),
	}, nil
}

func (d *DingTalk) Name() string {
	if d.name != "" {
		return d.name
	}
	return "dingtalk"
}

// SetName gives the instance its config name (multi-account support).
func (d *DingTalk) SetName(n string) {
	d.name = n
	// per-instance dedup file: two accounts of one platform must not
	// share a seen-store (they would swallow each other's messages)
	d.seen = dedup.New(strings.TrimSuffix(d.opts.DataDir, "/")+"/"+n+"-seen.json", 8*time.Hour, 5000)
}

func (d *DingTalk) Healthy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.healthy
}

func (d *DingTalk) Start(ctx context.Context, out chan<- channel.Message) error {
	ws := dingclient.NewStreamClient(
		dingclient.WithAppCredential(dingclient.NewAppCredentialConfig(d.opts.ClientID, d.opts.ClientSecret)),
	)
	ws.RegisterChatBotCallbackRouter(func(c context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
		// ack quickly; real processing happens on the pump
		msg, ok := d.toMessage(data)
		if !ok {
			return nil, nil
		}
		select {
		case out <- msg:
		case <-c.Done():
		case <-time.After(30 * time.Second):
		}
		return nil, nil // reply async via sessionWebhook, keeps the frame clean
	})
	d.mu.Lock()
	d.healthy = true
	d.mu.Unlock()
	err := ws.Start(ctx)
	d.mu.Lock()
	d.healthy = false
	d.mu.Unlock()
	return err
}

func (d *DingTalk) toMessage(data *chatbot.BotCallbackDataModel) (channel.Message, bool) {
	if data == nil || data.MsgId == "" {
		return channel.Message{}, false
	}
	if !d.seen.Claim(data.MsgId) {
		return channel.Message{}, false
	}
	if len(d.opts.AllowUsers) > 0 && !contains(d.opts.AllowUsers, data.SenderStaffId) {
		return channel.Message{}, false
	}
	chatType := "group"
	if data.ConversationType == "1" {
		chatType = "p2p"
	}
	text := strings.TrimSpace(data.Text.Content)
	return channel.Message{
		ID:        data.MsgId,
		Channel:   d.name,
		ChatID:    data.ConversationId,
		ChatType:  chatType,
		UserID:    data.SenderStaffId,
		UserName:  data.SenderNick,
		Text:      text,
		Raw:       data,
		Timestamp: time.UnixMilli(data.CreateAt),
	}, true
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ---- outbound via sessionWebhook ----

type Sender struct {
	d           *DingTalk
	sessionHook string
	expiry      int64
}

func (d *DingTalk) SenderFor(msg channel.Message) (channel.Sender, error) {
	raw, ok := msg.Raw.(*chatbot.BotCallbackDataModel)
	if !ok || raw.SessionWebhook == "" {
		return nil, fmt.Errorf("dingtalk: no sessionWebhook for chat %s", msg.ChatID)
	}
	return &Sender{d: d, sessionHook: raw.SessionWebhook, expiry: raw.SessionWebhookExpiredTime}, nil
}

func (s *Sender) Send(ctx context.Context, text string, opts channel.SendOptions) (string, error) {
	if s.expiry != 0 && time.Now().UnixMilli() > s.expiry {
		return "", fmt.Errorf("dingtalk: sessionWebhook expired")
	}
	body := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"title": "chatmux", "text": text},
		"at":       map[string]any{"isAtAll": false},
	}
	if len(opts.MentionUserIDs) > 0 {
		staffIDs := make([]string, 0, len(opts.MentionUserIDs))
		phoneFront := make([]string, 0, len(opts.MentionUserIDs))
		for _, id := range opts.MentionUserIDs {
			if strings.HasPrefix(id, "staff_") || strings.HasPrefix(id, "u_") {
				staffIDs = append(staffIDs, id)
			} else {
				// dingtalk @ needs staff ids; other id types degrade to text
				phoneFront = append(phoneFront, "@"+id)
			}
		}
		body["at"] = map[string]any{"atUserIds": staffIDs}
		if len(phoneFront) > 0 {
			body["markdown"].(map[string]string)["text"] = strings.Join(phoneFront, " ") + "\n\n" + text
		}
	}
	var out json.RawMessage
	if err := s.d.post(ctx, s.sessionHook, body, &out); err != nil {
		return "", err
	}
	return "", nil // webhook replies carry no message id
}

// Replace: DingTalk robot webhooks cannot edit sent messages — the
// runtime falls back to final-only delivery.
func (s *Sender) Replace(ctx context.Context, msgID, text string) error {
	return fmt.Errorf("dingtalk: message edit unsupported")
}

func (s *Sender) React(ctx context.Context, msgID, emoji string) error {
	return fmt.Errorf("dingtalk: reactions unsupported")
}

func (d *DingTalk) post(ctx context.Context, url string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("dingtalk post: %s %s", resp.Status, truncate(string(rb), 300))
	}
	if out != nil {
		return json.Unmarshal(rb, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
