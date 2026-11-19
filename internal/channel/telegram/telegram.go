// Package telegram is a zero-dependency Telegram Bot API channel using
// long polling (getUpdates). Works behind NAT, no public URL needed.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

type Options struct {
	Token      string   `json:"token"`      // bot token from @BotFather
	APIBase    string   `json:"apiBase"`    // default https://api.telegram.org
	AllowUsers []string `json:"allowUsers"` // empty = allow all
}

type Telegram struct {
	name         string // instance name (defaults to "telegram")
	opts         Options
	client       *http.Client
	feedbackHook func(kind, operator, msgID string)

	mu      sync.Mutex
	offset  int64
	healthy bool
	lastErr error
	names   map[int64]string // user id → username cache
}

func New(raw json.RawMessage) (channel.Channel, error) {
	var o Options
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("telegram options: %w", err)
	}
	if o.Token == "" {
		return nil, fmt.Errorf("telegram: token is required")
	}
	if o.APIBase == "" {
		o.APIBase = "https://api.telegram.org"
	}
	return &Telegram{
		name:   "telegram",
		opts:   o,
		client: &http.Client{Timeout: 70 * time.Second},
		names:  map[int64]string{},
	}, nil
}

func (t *Telegram) Name() string {
	if t.name != "" {
		return t.name
	}
	return "telegram"
}

// SetName gives the instance its config name (multi-account support).
func (t *Telegram) SetName(n string) { t.name = n }

// handleCallback answers 👍/👎 inline-keyboard presses.
func (t *Telegram) handleCallback(cb *struct {
	ID   string `json:"id"`
	From *struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"from"`
	Data    string `json:"data"`
	Message *struct {
		MessageID int64 `json:"message_id"`
		Chat      *struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}) {
	if cb == nil || !strings.HasPrefix(cb.Data, "cmx_fb_") {
		return
	}
	kind := strings.TrimPrefix(cb.Data, "cmx_fb_")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	toast := "已收到反馈 👌"
	if kind == "down" {
		toast = "已收到，下次改进 💪"
	}
	_ = t.call(ctx, "answerCallbackQuery", map[string]string{
		"callback_query_id": cb.ID, "text": toast,
	}, &struct {
		OK bool `json:"ok"`
	}{})
	if t.feedbackHook != nil {
		op := ""
		if cb.From != nil {
			op = strconv.FormatInt(cb.From.ID, 10)
		}
		go t.feedbackHook(kind, op, strconv.FormatInt(cb.Message.MessageID, 10))
	}
}

// SetFeedbackHook installs the 👍/👎 callback.
func (t *Telegram) SetFeedbackHook(fn func(kind, operator, msgID string)) { t.feedbackHook = fn }

func (t *Telegram) Healthy() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.healthy
}

type tgUpdate struct {
	UpdateID      int64 `json:"update_id"`
	CallbackQuery *struct {
		ID   string `json:"id"`
		From *struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
		Data    string `json:"data"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      *struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
	Message *struct {
		MessageID int64 `json:"message_id"`
		From      *struct {
			ID        int64  `json:"id"`
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"from"`
		Chat struct {
			ID    int64  `json:"id"`
			Type  string `json:"type"` // "private" | "group" | "supergroup"
			Title string `json:"title"`
		} `json:"chat"`
		ReplyToMessage *struct {
			MessageID int64  `json:"message_id"`
			Text      string `json:"text"`
			Caption   string `json:"caption"`
			From      *struct {
				ID        int64  `json:"id"`
				Username  string `json:"username"`
				FirstName string `json:"first_name"`
			} `json:"from"`
		} `json:"reply_to_message"`
		Text     string `json:"text"`
		Caption  string `json:"caption"`
		Entities []struct {
			Type   string `json:"type"`
			Offset int    `json:"offset"`
			Length int    `json:"length"`
		} `json:"entities"`
		Photo []struct {
			FileID string `json:"file_id"`
		} `json:"photo"`
		Voice *struct {
			FileID string `json:"file_id"`
		} `json:"voice"`
		Audio *struct {
			FileID   string `json:"file_id"`
			FileName string `json:"file_name"`
		} `json:"audio"`
		Document *struct {
			FileID   string `json:"file_id"`
			FileName string `json:"file_name"`
			MimeType string `json:"mime_type"`
		} `json:"document"`
	} `json:"message"`
}

func (t *Telegram) Start(ctx context.Context, out chan<- channel.Message) error {
	// drop pending updates from before startup
	var me struct {
		OK bool `json:"ok"`
	}
	if err := t.call(ctx, "getMe", nil, &me); err != nil {
		return fmt.Errorf("telegram getMe: %w", err)
	}
	t.setHealthy(true, nil)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		var ups struct {
			OK     bool       `json:"ok"`
			Result []tgUpdate `json:"result"`
		}
		body := map[string]any{"timeout": 50, "allowed_updates": []string{"message", "callback_query"}}
		t.mu.Lock()
		if t.offset != 0 {
			body["offset"] = t.offset
		}
		t.mu.Unlock()
		if err := t.call(ctx, "getUpdates", body, &ups); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			t.setHealthy(false, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(3 * time.Second):
			}
			continue
		}
		t.setHealthy(true, nil)
		for _, u := range ups.Result {
			t.mu.Lock()
			t.offset = u.UpdateID + 1
			t.mu.Unlock()
			if u.CallbackQuery != nil {
				t.handleCallback(u.CallbackQuery)
				continue
			}
			if msg, ok := t.toMessage(u); ok {
				select {
				case out <- msg:
				case <-ctx.Done():
					return nil
				}
			}
		}
	}
}

func (t *Telegram) toMessage(u tgUpdate) (channel.Message, bool) {
	m := u.Message
	if m == nil || (m.Text == "" && m.Caption == "" && len(m.Photo) == 0 && m.Document == nil) {
		return channel.Message{}, false
	}
	if len(t.opts.AllowUsers) > 0 && m.From != nil {
		allowed := false
		for _, a := range t.opts.AllowUsers {
			if a == m.From.Username || a == strconv.FormatInt(m.From.ID, 10) {
				allowed = true
			}
		}
		if !allowed {
			return channel.Message{}, false
		}
	}
	chatType := "group"
	if m.Chat.Type == "private" {
		chatType = "p2p"
	}

	msg := channel.Message{
		ID:        strconv.FormatInt(m.MessageID, 10),
		Channel:   t.name,
		ChatID:    strconv.FormatInt(m.Chat.ID, 10),
		ChatType:  chatType,
		UserID:    strconv.FormatInt(m.From.ID, 10),
		UserName:  displayName(m.From),
		Text:      m.Text,
		Timestamp: time.Now(),
	}
	if m.ReplyToMessage != nil {
		msg.ThreadID = strconv.FormatInt(m.ReplyToMessage.MessageID, 10)
		if qt := firstNonEmptyT(m.ReplyToMessage.Text, m.ReplyToMessage.Caption); qt != "" {
			q := &channel.Quoted{
				ID:   strconv.FormatInt(m.ReplyToMessage.MessageID, 10),
				Text: qt,
			}
			if m.ReplyToMessage.From != nil {
				q.UserID = strconv.FormatInt(m.ReplyToMessage.From.ID, 10)
				q.UserName = m.ReplyToMessage.From.Username
				if q.UserName == "" {
					q.UserName = m.ReplyToMessage.From.FirstName
				}
			}
			msg.Quoted = q
		}
	}
	// mention stripping: entities carry bot_command/mention offsets over
	// UTF-16 code units — strip leading commands only, keep it simple.
	for _, e := range m.Entities {
		if e.Type == "bot_command" && e.Offset == 0 {
			msg.Text = strings.TrimSpace(msg.Text[e.Length:])
		}
	}
	if m.Voice != nil {
		msg.Attachments = append(msg.Attachments, channel.Attachment{
			Type: "audio", Name: "voice.ogg", Key: m.Voice.FileID,
		})
	}
	if m.Audio != nil {
		msg.Attachments = append(msg.Attachments, channel.Attachment{
			Type: "audio", Name: m.Audio.FileName, Key: m.Audio.FileID,
		})
	}
	if len(m.Photo) > 0 {
		msg.Attachments = append(msg.Attachments, channel.Attachment{
			Type: "image", Name: "photo.jpg",
			Key: m.Photo[len(m.Photo)-1].FileID,
		})
	}
	if m.Document != nil {
		kind := "file"
		if strings.HasPrefix(m.Document.MimeType, "image/") {
			kind = "image"
		}
		msg.Attachments = append(msg.Attachments, channel.Attachment{
			Type: kind, Name: m.Document.FileName,
			Key: m.Document.FileID,
		})
	}
	return msg, true
}

func firstNonEmptyT(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func displayName(f *struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}) string {
	if f == nil {
		return "?"
	}
	if f.Username != "" {
		return f.Username
	}
	return f.FirstName
}

func (t *Telegram) fileURL(fileID string) string {
	// unused placeholder kept for compatibility; Download below is the
	// real two-step fetch (getFile → file/bot<token>/<path>).
	_ = fileID
	return ""
}

// Download implements channel.AttachmentDownloader: getFile resolves the
// file_path, then the file endpoint serves the bytes.
func (t *Telegram) Download(ctx context.Context, att channel.Attachment) ([]byte, string, error) {
	if att.Key == "" {
		return nil, "", fmt.Errorf("telegram: attachment has no file id")
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := t.call(ctx, "getFile", map[string]string{"file_id": att.Key}, &out); err != nil {
		return nil, "", err
	}
	if out.Result.FilePath == "" {
		return nil, "", fmt.Errorf("telegram: empty file_path for %s", att.Key)
	}
	url := fmt.Sprintf("%s/file/bot%s/%s", t.opts.APIBase, t.opts.Token, out.Result.FilePath)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("telegram file download: %s", resp.Status)
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		mime = http.DetectContentType(data)
	}
	return data, mime, nil
}

func (t *Telegram) setHealthy(ok bool, err error) {
	t.mu.Lock()
	t.healthy, t.lastErr = ok, err
	t.mu.Unlock()
}

// ---- outbound ----

type Sender struct {
	t      *Telegram
	chatID string
}

func (t *Telegram) SenderFor(msg channel.Message) (channel.Sender, error) {
	return &Sender{t: t, chatID: msg.ChatID}, nil
}

const telegramMaxLen = 4000

func (s *Sender) Send(ctx context.Context, text string, opts channel.SendOptions) (string, error) {
	// split long messages into chunks, telegram hard-caps ~4096
	chunks := splitMessage(text, telegramMaxLen)
	var lastID string
	for i, c := range chunks {
		body := map[string]any{
			"chat_id": s.chatID,
			"text":    c,
		}
		if opts.ReplyTo != "" && i == 0 {
			body["reply_to_message_id"] = mustInt(opts.ReplyTo)
		}
		if len(opts.MentionUserIDs) > 0 && i == len(chunks)-1 {
			body["text"] = c + "\n" + mentionLine(opts.MentionUserIDs)
		}
		if !opts.Final {
			body["disable_notification"] = true
		} else if i == len(chunks)-1 {
			// 👍/👎 inline keyboard on the final chunk
			body["reply_markup"] = map[string]any{
				"inline_keyboard": [][]map[string]string{
					{{"text": "👍", "callback_data": "cmx_fb_up"}, {"text": "👎", "callback_data": "cmx_fb_down"}},
				},
			}
		}
		var res struct {
			OK     bool `json:"ok"`
			Result struct {
				MessageID int64 `json:"message_id"`
			} `json:"result"`
		}
		if err := s.t.call(ctx, "sendMessage", body, &res); err != nil {
			return lastID, err
		}
		lastID = strconv.FormatInt(res.Result.MessageID, 10)
	}
	return lastID, nil
}

func (s *Sender) Replace(ctx context.Context, msgID, text string) error {
	body := map[string]any{
		"chat_id":    s.chatID,
		"message_id": mustInt(msgID),
		"text":       text,
	}
	var res struct {
		OK bool `json:"ok"`
	}
	return s.t.call(ctx, "editMessageText", body, &res)
}

func (s *Sender) React(ctx context.Context, msgID, emoji string) error {
	body := map[string]any{
		"chat_id":    s.chatID,
		"message_id": mustInt(msgID),
		"reaction":   []map[string]string{{"type": "emoji", "emoji": emoji}},
	}
	var res struct {
		OK bool `json:"ok"`
	}
	return s.t.call(ctx, "setMessageReaction", body, &res)
}

func mentionLine(ids []string) string {
	// telegram mentions users via inline markup; without usernames the
	// API needs entities — approximate with plain ids as text mentions
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = "[user" + id + "](tg://user?id=" + id + ")"
	}
	return strings.Join(parts, " ")
}

func mustInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func splitMessage(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}
	var chunks []string
	for len(text) > max {
		cut := max
		// prefer cutting at newline near the limit
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

// ---- api ----

func (t *Telegram) call(ctx context.Context, method string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/bot%s/%s", t.opts.APIBase, t.opts.Token, method), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram %s: %s %s", method, resp.Status, string(b[:min(len(b), 300)]))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
