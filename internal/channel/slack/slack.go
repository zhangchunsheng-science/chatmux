// Package slack is a Socket Mode channel: no public URL needed, one
// websocket to Slack plus REST for sending/editing.
//
// Setup: app with Bot Token (xoxb-), App-Level Token (xapp- with
// connections:write), Event Subscriptions on message.channels /
// message.im / app_mention, chat:write scope.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/dedup"
)

type Options struct {
	BotToken   string   `json:"botToken"` // xoxb-
	AppToken   string   `json:"appToken"` // xapp-
	APIBase    string   `json:"apiBase"`  // default https://slack.com
	WSProxy    string   `json:"wsProxy"`  // optional wss proxy url
	DataDir    string   `json:"dataDir"`
	AllowUsers []string `json:"allowUsers"`
}

type Slack struct {
	name         string // instance name (defaults to "slack")
	opts         Options
	client       *http.Client
	seen         *dedup.Store
	botID        string
	feedbackHook func(kind, operator, msgID string)

	mu      sync.Mutex
	healthy bool
}

// SetFeedbackHook installs the 👍/👍 callback.
func (s *Slack) SetFeedbackHook(fn func(kind, operator, msgID string)) { s.feedbackHook = fn }

func New(raw json.RawMessage) (channel.Channel, error) {
	var o Options
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("slack options: %w", err)
	}
	if o.BotToken == "" || o.AppToken == "" {
		return nil, fmt.Errorf("slack: botToken and appToken are required")
	}
	if o.APIBase == "" {
		o.APIBase = "https://slack.com"
	}
	if o.DataDir == "" {
		o.DataDir = "data"
	}
	return &Slack{
		name:   "slack",
		opts:   o,
		client: &http.Client{Timeout: 30 * time.Second},
		seen:   dedup.New(strings.TrimSuffix(o.DataDir, "/")+"/slack-seen.json", 8*time.Hour, 5000),
	}, nil
}

func (s *Slack) Name() string {
	if s.name != "" {
		return s.name
	}
	return "slack"
}

// SetName gives the instance its config name (multi-account support).
func (s *Slack) SetName(n string) {
	s.name = n
	// per-instance dedup file: two accounts of one platform must not
	// share a seen-store (they would swallow each other's messages)
	s.seen = dedup.New(strings.TrimSuffix(s.opts.DataDir, "/")+"/"+n+"-seen.json", 8*time.Hour, 5000)
}

func (s *Slack) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}

func (s *Slack) setHealthy(v bool) {
	s.mu.Lock()
	s.healthy = v
	s.mu.Unlock()
}

// ---- socket mode ----

type smEnvelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	// hello/debug/disconnect/_unknown + reconnect
	Reason    string `json:"reason"`
	DebugInfo struct {
		Count int `json:"reconnect_count"`
	} `json:"debug_info"`
}

func (s *Slack) Start(ctx context.Context, out chan<- channel.Message) error {
	if s.botID == "" {
		var auth struct {
			OK  bool `json:"ok"`
			Bot struct {
				ID string `json:"bot_id"`
			} `json:"bot"`
			User struct {
				ID string `json:"user_id"`
			} `json:"user"`
		}
		if err := s.api(ctx, "auth.test", nil, &auth); err == nil && auth.OK {
			s.botID = auth.User.ID
		}
	}

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	for {
		_ = s.runSocketMode(ctx, &dialer, out)
		if ctx.Err() != nil {
			return nil
		}
		s.setHealthy(false)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

func (s *Slack) runSocketMode(ctx context.Context, dialer *websocket.Dialer, out chan<- channel.Message) error {
	wss, err := s.wsURL(ctx)
	if err != nil {
		return err
	}
	header := http.Header{"Origin": []string{"https://api.slack.com"}}
	conn, _, err := dialer.DialContext(ctx, wss, header)
	if err != nil {
		return fmt.Errorf("slack socket dial: %w", err)
	}
	defer conn.Close()
	s.setHealthy(true)

	type reader struct {
		typ int
		raw json.RawMessage
	}
	msgs := make(chan reader, 16)
	go func() {
		defer close(msgs)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case msgs <- reader{mt, data}:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case r, ok := <-msgs:
			if !ok {
				return fmt.Errorf("slack socket closed")
			}
			if r.typ != websocket.TextMessage {
				continue
			}
			var env smEnvelope
			if json.Unmarshal(r.raw, &env) != nil {
				continue
			}
			if env.EnvelopeID != "" {
				// ack everything immediately: redelivery would dup anyway
				ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
				_ = conn.WriteMessage(websocket.TextMessage, ack)
			}
			switch env.Type {
			case "events_api":
				s.handleEvent(ctx, env.Payload, out)
			case "interactive":
				s.handleInteractive(env.Payload)
			case "disconnect", "reconnect":
				return fmt.Errorf("slack asked reconnect: %s", env.Reason)
			}
		}
	}
}

// handleInteractive answers 👍/👎 block-action presses.
func (s *Slack) handleInteractive(payload json.RawMessage) {
	var it struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Actions []struct {
			ActionID string `json:"action_id"`
		} `json:"actions"`
		Message struct {
			TS string `json:"ts"`
		} `json:"message"`
		ResponseURL string `json:"response_url"`
	}
	if json.Unmarshal(payload, &it) != nil || len(it.Actions) == 0 {
		return
	}
	if !strings.HasPrefix(it.Actions[0].ActionID, "cmx_fb_") {
		return
	}
	kind := strings.TrimPrefix(it.Actions[0].ActionID, "cmx_fb_")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if it.ResponseURL != "" {
		// ephemeral ack via response_url
		text := "已收到反馈 👌"
		if kind == "down" {
			text = "已收到，下次改进 💪"
		}
		b, _ := json.Marshal(map[string]any{
			"response_type": "ephemeral", "replace_original": false,
			"text": text,
		})
		req, err := http.NewRequestWithContext(ctx, "POST", it.ResponseURL, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			resp, err := s.client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}
	if s.feedbackHook != nil {
		go s.feedbackHook(kind, it.User.ID, it.Message.TS)
	}
}

func (s *Slack) handleEvent(ctx context.Context, payload json.RawMessage, out chan<- channel.Message) {
	var ev struct {
		Event struct {
			Type        string `json:"type"`
			Channel     string `json:"channel"`
			ChannelType string `json:"channel_type"`
			User        string `json:"user"`
			Text        string `json:"text"`
			TS          string `json:"ts"`
			ThreadTS    string `json:"thread_ts"`
			Files       []struct {
				URLPrivate string `json:"url_private"`
				Name       string `json:"name"`
				Filetype   string `json:"filetype"`
			} `json:"files"`
			BotID string `json:"bot_id"`
		} `json:"event"`
		EventID string `json:"event_id"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	e := ev.Event
	if e.BotID != "" || e.User == "" {
		return // skip bots (incl. self)
	}
	if e.Type != "message" && e.Type != "app_mention" {
		return
	}
	if !s.seen.Claim(ev.EventID + "|" + e.TS) {
		return
	}
	if len(s.opts.AllowUsers) > 0 && !contains(s.opts.AllowUsers, e.User) {
		return
	}
	chatType := "group"
	if e.ChannelType == "im" {
		chatType = "p2p"
	}
	text := e.Text
	// strip <@U123> mention tags into plain ids
	text = expandMentions(text)
	mentionedMe := strings.Contains(e.Text, "<@"+s.botID)
	msg := channel.Message{
		ID:          e.TS,
		Channel:     s.name,
		ChatID:      e.Channel,
		ChatType:    chatType,
		ThreadID:    e.ThreadTS,
		UserID:      e.User,
		Text:        strings.TrimSpace(text),
		MentionedMe: mentionedMe,
		Timestamp:   time.Now(),
	}
	// thread reply: quote the thread-root text for context
	if e.ThreadTS != "" && e.ThreadTS != e.TS {
		if q, ok := s.threadRoot(e.Channel, e.ThreadTS); ok {
			msg.Quoted = q
		}
	}
	for _, f := range e.Files {
		if strings.HasPrefix(f.Filetype, "image") {
			msg.Attachments = append(msg.Attachments, channel.Attachment{
				Type: "image", URL: f.URLPrivate, Name: f.Name,
			})
		} else {
			msg.Attachments = append(msg.Attachments, channel.Attachment{
				Type: "file", URL: f.URLPrivate, Name: f.Name,
			})
		}
	}
	if msg.Text == "" && len(msg.Attachments) == 0 {
		return
	}
	select {
	case out <- msg:
	case <-ctx.Done():
	}
}

func expandMentions(text string) string {
	// <@U1234> → @U1234 ; <#C123|name> → #name ; <url|label> → label
	var b strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '<' {
			if j := strings.IndexByte(text[i:], '>'); j > 0 {
				inner := text[i+1 : i+j]
				i += j + 1
				switch {
				case strings.HasPrefix(inner, "@"):
					b.WriteString("@" + strings.TrimPrefix(inner, "@"))
				case strings.HasPrefix(inner, "#"):
					parts := strings.SplitN(inner, "|", 2)
					if len(parts) == 2 {
						b.WriteString("#" + parts[1])
					} else {
						b.WriteString("#" + strings.TrimPrefix(parts[0], "#"))
					}
				default:
					parts := strings.SplitN(inner, "|", 2)
					if len(parts) == 2 {
						b.WriteString(parts[1])
					} else {
						b.WriteString(inner)
					}
				}
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Slack) wsURL(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST",
		s.opts.APIBase+"/api/apps.connections.open", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+s.opts.AppToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var res struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if !res.OK || res.URL == "" {
		return "", fmt.Errorf("slack connections.open: %s", res.Error)
	}
	return res.URL, nil
}

// ---- outbound ----

type Sender struct {
	s      *Slack
	chatID string
}

func (s *Slack) SenderFor(msg channel.Message) (channel.Sender, error) {
	return &Sender{s: s, chatID: msg.ChatID}, nil
}

func (s *Sender) Send(ctx context.Context, text string, opts channel.SendOptions) (string, error) {
	body := map[string]any{"channel": s.chatID, "text": text}
	if opts.ReplyTo != "" {
		body["thread_ts"] = opts.ReplyTo
	}
	if opts.Final {
		// 👍/👎 buttons as a block action row
		body["blocks"] = []any{
			map[string]any{"type": "section", "text": map[string]any{
				"type": "mrkdwn", "text": text,
			}},
			map[string]any{"type": "actions", "elements": []any{
				map[string]any{"type": "button", "action_id": "cmx_fb_up",
					"text": map[string]any{"type": "plain_text", "text": "👍"}},
				map[string]any{"type": "button", "action_id": "cmx_fb_down",
					"text": map[string]any{"type": "plain_text", "text": "👎"}},
			}},
		}
	}
	var res struct {
		OK    bool   `json:"ok"`
		TS    string `json:"ts"`
		Error string `json:"error"`
	}
	if err := s.s.api(ctx, "chat.postMessage", body, &res); err != nil {
		return "", err
	}
	return res.TS, nil
}

func (s *Sender) Replace(ctx context.Context, msgID, text string) error {
	body := map[string]any{"channel": s.chatID, "ts": msgID, "text": text}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := s.s.api(ctx, "chat.update", body, &res); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("slack chat.update: %s", res.Error)
	}
	return nil
}

func (s *Sender) React(ctx context.Context, msgID, emoji string) error {
	body := map[string]any{"channel": s.chatID, "timestamp": msgID, "name": emoji}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := s.s.api(ctx, "reactions.add", body, &res); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("slack reactions.add: %s", res.Error)
	}
	return nil
}

// threadRoot fetches a thread's root message as a Quoted context block.
func (s *Slack) threadRoot(channelID, threadTS string) (*channel.Quoted, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out struct {
		OK       bool `json:"ok"`
		Messages []struct {
			Ts      string `json:"ts"`
			Text    string `json:"text"`
			User    string `json:"user"`
			SubType string `json:"subtype"`
		} `json:"messages"`
	}
	err := s.api(ctx, "conversations.history", map[string]string{
		"channel": channelID, "inclusive": "true",
		"oldest": threadTS, "latest": threadTS, "limit": "1",
	}, &out)
	if err != nil || !out.OK || len(out.Messages) == 0 {
		return nil, false
	}
	root := out.Messages[0]
	if strings.TrimSpace(root.Text) == "" || root.User == "" || root.User == s.botID {
		return nil, false
	}
	return &channel.Quoted{ID: root.Ts, UserID: root.User, Text: expandMentions(root.Text)}, true
}

func (s *Slack) api(ctx context.Context, method string, body any, out any) error {
	form := url.Values{}
	if body != nil {
		b, _ := json.Marshal(body)
		var flat map[string]any
		_ = json.Unmarshal(b, &flat)
		for k, v := range flat {
			switch vv := v.(type) {
			case string:
				form.Set(k, vv)
			default:
				jb, _ := json.Marshal(v)
				form.Set(k, string(jb))
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		s.opts.APIBase+"/api/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.opts.BotToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack %s: %s %s", method, resp.Status, string(b[:min(len(b), 300)]))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
