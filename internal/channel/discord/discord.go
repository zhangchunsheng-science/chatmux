// Package discord is a raw Discord gateway channel: websocket to the
// gateway for events (MESSAGE_CREATE / DirectMessage), REST for send and
// edit. Bot needs the Message Content intent enabled.
package discord

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

	"github.com/gorilla/websocket"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/dedup"
)

type Options struct {
	Token      string   `json:"token"` // bot token
	APIBase    string   `json:"apiBase"`
	Gateway    string   `json:"gateway"` // default wss://gateway.discord.gg/?v=10&encoding=json
	DataDir    string   `json:"dataDir"`
	AllowUsers []string `json:"allowUsers"`
}

type Discord struct {
	name   string // instance name (defaults to "discord")
	opts   Options
	client *http.Client
	seen   *dedup.Store
	botID  string

	mu      sync.Mutex
	healthy bool
}

func New(raw json.RawMessage) (channel.Channel, error) {
	var o Options
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("discord options: %w", err)
	}
	if o.Token == "" {
		return nil, fmt.Errorf("discord: token is required")
	}
	if o.APIBase == "" {
		o.APIBase = "https://discord.com/api/v10"
	}
	if o.Gateway == "" {
		o.Gateway = "wss://gateway.discord.gg/?v=10&encoding=json"
	}
	if o.DataDir == "" {
		o.DataDir = "data"
	}
	return &Discord{
		name:   "discord",
		opts:   o,
		client: &http.Client{Timeout: 30 * time.Second},
		seen:   dedup.New(strings.TrimSuffix(o.DataDir, "/")+"/discord-seen.json", 8*time.Hour, 5000),
	}, nil
}

func (d *Discord) Name() string {
	if d.name != "" {
		return d.name
	}
	return "discord"
}

// SetName gives the instance its config name (multi-account support).
func (d *Discord) SetName(n string) { d.name = n }

func (d *Discord) Healthy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.healthy
}

const intents = 1 | 512 | 32768 | 8192 // Guilds | GuildMessages | MessageContent | DirectMessages

type gwPayload struct {
	Op   int             `json:"op"`
	Data json.RawMessage `json:"d"`
	Seq  *int64          `json:"s"`
	Type string          `json:"t"`
}

func (d *Discord) Start(ctx context.Context, out chan<- channel.Message) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	var lastSeq int64
	sessionID := ""
	for {
		_ = d.runGateway(ctx, &dialer, out, &lastSeq, &sessionID)
		if ctx.Err() != nil {
			return nil
		}
		d.mu.Lock()
		d.healthy = false
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

func (d *Discord) runGateway(ctx context.Context, dialer *websocket.Dialer, out chan<- channel.Message, lastSeq *int64, sessionID *string) error {
	conn, _, err := dialer.DialContext(ctx, d.opts.Gateway, nil)
	if err != nil {
		return fmt.Errorf("discord dial: %w", err)
	}
	defer conn.Close()

	var heartbeat time.Duration
	var heartbeatTick *time.Ticker
	defer func() {
		if heartbeatTick != nil {
			heartbeatTick.Stop()
		}
	}()

	type frame struct {
		op   int
		typ  string
		data json.RawMessage
	}
	frames := make(chan frame, 32)
	readErr := make(chan error, 1)
	go func() {
		defer close(frames)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var p gwPayload
			if json.Unmarshal(data, &p) != nil {
				continue
			}
			if p.Seq != nil {
				*lastSeq = *p.Seq
			}
			select {
			case frames <- frame{p.Op, p.Type, p.Data}:
			case <-ctx.Done():
				return
			}
		}
	}()

	send := func(op int, v any) error {
		b, _ := json.Marshal(map[string]any{"op": op, "d": v})
		return conn.WriteMessage(websocket.TextMessage, b)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return fmt.Errorf("discord read: %w", err)
		case <-func() <-chan time.Time {
			if heartbeatTick == nil {
				return time.After(365 * 24 * time.Hour) // blocked until hello
			}
			return heartbeatTick.C
		}():
			_ = send(1, *lastSeq)
		case f, ok := <-frames:
			if !ok {
				return fmt.Errorf("discord socket closed")
			}
			switch f.op {
			case 10: // HELLO
				var h struct {
					HeartbeatInterval int `json:"heartbeat_interval"`
				}
				_ = json.Unmarshal(f.data, &h)
				heartbeat = time.Duration(h.HeartbeatInterval) * time.Millisecond
				iv := heartbeat - 2*time.Second
				if iv <= 0 {
					iv = heartbeat / 2
				}
				heartbeatTick = time.NewTicker(iv)
				if *sessionID != "" {
					// RESUME
					_ = send(6, map[string]any{
						"token": d.opts.Token, "session_id": *sessionID, "seq": *lastSeq,
					})
				} else {
					// IDENTIFY
					_ = send(2, map[string]any{
						"token":      d.opts.Token,
						"intents":    intents,
						"properties": map[string]string{"os": "darwin", "browser": "chatmux", "device": "chatmux"},
					})
				}
			case 0: // DISPATCH
				switch f.typ {
				case "READY":
					var t struct {
						SessionID string `json:"session_id"`
						User      struct {
							ID string `json:"id"`
						} `json:"user"`
					}
					_ = json.Unmarshal(f.data, &t)
					*sessionID = t.SessionID
					d.mu.Lock()
					d.botID = t.User.ID
					d.healthy = true
					d.mu.Unlock()
				case "MESSAGE_CREATE", "DIRECT_MESSAGE_CREATE":
					if msg, ok := d.toMessage(f.data); ok {
						select {
						case out <- msg:
						case <-ctx.Done():
							return nil
						}
					}
				}
			case 7: // reconnect request
				return fmt.Errorf("discord requested reconnect")
			case 9: // invalid session
				*sessionID = ""
				_ = send(2, map[string]any{
					"token": d.opts.Token, "intents": intents,
					"properties": map[string]string{"os": "darwin", "browser": "chatmux", "device": "chatmux"},
				})
			case 11: // heartbeat ack
			}
		}
	}
}

func (d *Discord) toMessage(data json.RawMessage) (channel.Message, bool) {
	var m struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		Author    struct {
			ID       string `json:"id"`
			Bot      bool   `json:"bot"`
			Username string `json:"username"`
		} `json:"author"`
		Content   string `json:"content"`
		Type      int    `json:"type"`
		Reference *struct {
			ID      string `json:"id"`
			Content string `json:"content"`
			Author  struct {
				ID       string `json:"id"`
				Bot      bool   `json:"bot"`
				Username string `json:"username"`
			} `json:"author"`
		} `json:"referenced_message"`
		Attachments []struct {
			URL      string `json:"url"`
			Filename string `json:"filename"`
		} `json:"attachments"`
	}
	if json.Unmarshal(data, &m) != nil {
		return channel.Message{}, false
	}
	if m.Author.Bot || m.Author.ID == "" || (m.Content == "" && len(m.Attachments) == 0) {
		return channel.Message{}, false
	}
	if !d.seen.Claim(m.ID + "|" + m.Author.ID) {
		return channel.Message{}, false
	}
	if len(d.opts.AllowUsers) > 0 && !contains(d.opts.AllowUsers, m.Author.ID) {
		return channel.Message{}, false
	}
	d.mu.Lock()
	botID := d.botID
	d.mu.Unlock()

	mentionedMe := botID != "" && strings.Contains(m.Content, "<@"+botID+">")
	content := m.Content
	// strip raw mention tags <@id> → @id
	content = strings.ReplaceAll(content, "<@", "@")
	content = strings.ReplaceAll(content, ">", "")

	msg := channel.Message{
		ID:          m.ID,
		Channel:     d.name,
		ChatID:      m.ChannelID,
		ChatType:    "group", // DM channels look the same at rest level; fine
		UserID:      m.Author.ID,
		UserName:    m.Author.Username,
		Text:        strings.TrimSpace(content),
		MentionedMe: mentionedMe,
		Timestamp:   time.Now(),
	}
	// reply quote: referenced_message carries the quoted post verbatim
	if m.Type == 19 && m.Reference != nil { // 19 = REPLY
		msg.ThreadID = m.Reference.ID
		if m.Reference.Content != "" && !m.Reference.Author.Bot {
			msg.Quoted = &channel.Quoted{
				ID:       m.Reference.ID,
				UserID:   m.Reference.Author.ID,
				UserName: m.Reference.Author.Username,
				Text:     strings.TrimSpace(m.Reference.Content),
			}
		}
	}
	for _, a := range m.Attachments {
		kind := "file"
		if strings.Contains(a.URL, ".png") || strings.Contains(a.URL, ".jpg") || strings.Contains(a.URL, ".jpeg") || strings.Contains(a.URL, ".webp") || strings.Contains(a.URL, ".gif") {
			kind = "image"
		}
		msg.Attachments = append(msg.Attachments, channel.Attachment{Type: kind, URL: a.URL, Name: a.Filename})
	}
	return msg, true
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ---- outbound ----

type Sender struct {
	d      *Discord
	chatID string
}

func (d *Discord) SenderFor(msg channel.Message) (channel.Sender, error) {
	return &Sender{d: d, chatID: msg.ChatID}, nil
}

const discordMaxLen = 2000

func (s *Sender) Send(ctx context.Context, text string, opts channel.SendOptions) (string, error) {
	chunks := chunk(text, discordMaxLen)
	var lastID string
	for i, c := range chunks {
		body := map[string]any{"content": c}
		if i == 0 && opts.ReplyTo != "" {
			body["message_reference"] = map[string]string{"message_id": opts.ReplyTo}
		}
		var res struct {
			ID string `json:"id"`
		}
		if err := s.d.rest(ctx, "POST", "/channels/"+s.chatID+"/messages", body, &res); err != nil {
			return lastID, err
		}
		lastID = res.ID
	}
	return lastID, nil
}

func (s *Sender) Replace(ctx context.Context, msgID, text string) error {
	body := map[string]any{"content": text}
	return s.d.rest(ctx, "PATCH", "/channels/"+s.chatID+"/messages/"+msgID, body, nil)
}

func (s *Sender) React(ctx context.Context, msgID, emoji string) error {
	return s.d.rest(ctx, "PUT",
		"/channels/"+s.chatID+"/messages/"+msgID+"/reactions/"+emoji+"/@me", nil, nil)
}

func (d *Discord) rest(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.opts.APIBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+d.opts.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("discord %s: %s %s", path, resp.Status, string(b[:min(len(b), 300)]))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func chunk(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}
	var chunks []string
	for len(text) > max {
		cut := max
		if i := strings.LastIndexByte(text[:max], '\n'); i > max/2 {
			cut = i
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if strings.TrimSpace(text) != "" {
		chunks = append(chunks, text)
	}
	return chunks
}
