// Package wecom is the 企业微信 (WeCom) self-built-app channel:
// encrypted HTTP callback for inbound messages + REST send for replies.
//
// Setup: create a self-built app (自建应用) in the WeCom admin console,
// get CorpID + AgentID + Secret, configure "receive message" (接收消息)
// callback URL/Token/EncodingAESKey pointing at this process, and enable
// API message receive (api收消息). No public edits/reactions exist, so
// streaming Replace is unsupported (final message only).
package wecom

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/dedup"
)

type Options struct {
	CorpID         string   `json:"corpId"`
	AgentID        int      `json:"agentId"`
	Secret         string   `json:"secret"`
	Token          string   `json:"token"`          // callback Token
	EncodingAESKey string   `json:"encodingAesKey"` // callback EncodingAESKey
	CallbackPath   string   `json:"callbackPath"`   // default /wecom/callback
	APIBase        string   `json:"apiBase"`        // default https://qyapi.weixin.qq.com
	DataDir        string   `json:"dataDir"`        // dedup store dir
	AllowUsers     []string `json:"allowUsers"`     // empty = allow all
}

type WeCom struct {
	name   string // instance name (defaults to "wecom")
	opts   Options
	crypto *cbCrypto
	client *http.Client
	seen   *dedup.Store

	mu      sync.Mutex
	accTok  string
	tokExp  time.Time
	healthy bool
	pump    chan<- channel.Message
}

func New(raw json.RawMessage) (channel.Channel, error) {
	var o Options
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("wecom options: %w", err)
	}
	if o.CorpID == "" || o.Secret == "" || o.Token == "" || o.EncodingAESKey == "" {
		return nil, fmt.Errorf("wecom: corpId, secret, token, encodingAesKey are required")
	}
	if o.CallbackPath == "" {
		o.CallbackPath = "/wecom/callback"
	}
	if o.APIBase == "" {
		o.APIBase = "https://qyapi.weixin.qq.com"
	}
	if o.DataDir == "" {
		o.DataDir = "data"
	}
	c, err := newCBCrypto(o.Token, o.EncodingAESKey, o.CorpID)
	if err != nil {
		return nil, err
	}
	return &WeCom{
		name:   "wecom",
		opts:   o,
		crypto: c,
		client: &http.Client{Timeout: 30 * time.Second},
		seen:   dedup.New(filepath.Join(o.DataDir, "wecom-seen.json"), 8*time.Hour, 5000),
	}, nil
}

func (w *WeCom) Name() string {
	if w.name != "" {
		return w.name
	}
	return "wecom"
}

// SetName gives the instance its config name (multi-account support).
func (w *WeCom) SetName(n string) {
	w.name = n
	// per-instance dedup file: two accounts of one platform must not
	// share a seen-store (they would swallow each other's messages)
	w.seen = dedup.New(filepath.Join(w.opts.DataDir, n+"-seen.json"), 8*time.Hour, 5000)
}

func (w *WeCom) Healthy() bool { return true } // webhook channel: healthy if process is up

// Endpoints mounts the callback receiver.
func (w *WeCom) Endpoints() map[string]http.Handler {
	return map[string]http.Handler{w.opts.CallbackPath: http.HandlerFunc(w.serveCallback)}
}

type cbRequest struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	Encrypt    string   `xml:"Encrypt"`
	AgentID    string   `xml:"AgentID"`
}

type cbPlain struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
	PicURL       string   `xml:"PicUrl"`
	MediaID      string   `xml:"MediaId"`
	ChatID       string   `xml:"ChatId"`
	AgentID      int      `xml:"AgentID"`
	Event        string   `xml:"Event"`
	ChangeType   string   `xml:"ChangeType"`
	UpdateDetail string   `xml:"UpdateDetail"`
	MsgID        string   `xml:"MsgId"`
	// async ack of our sends (later versions): ignore
}

func (w *WeCom) serveCallback(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	msgSignature, timestamp, nonce := q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce")
	if r.Method == http.MethodGet {
		// URL verification: echostr decrypts to the plain echo text
		echo, err := w.crypto.decrypt(q.Get("echostr"))
		if err != nil || w.crypto.signature(timestamp, nonce, q.Get("echostr")) != msgSignature {
			http.Error(rw, "verify failed", http.StatusForbidden)
			return
		}
		_, _ = rw.Write([]byte(echo))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(rw, "bad body", http.StatusBadRequest)
		return
	}
	var creq cbRequest
	if err := xml.Unmarshal(body, &creq); err != nil {
		http.Error(rw, "bad xml", http.StatusBadRequest)
		return
	}
	if w.crypto.signature(timestamp, nonce, creq.Encrypt) != msgSignature {
		http.Error(rw, "bad signature", http.StatusForbidden)
		return
	}
	plainXML, err := w.crypto.decrypt(creq.Encrypt)
	if err != nil {
		http.Error(rw, "decrypt failed", http.StatusForbidden)
		return
	}
	var p cbPlain
	if err := xml.Unmarshal([]byte(plainXML), &p); err != nil {
		_, _ = rw.Write([]byte("success"))
		return
	}
	_, _ = rw.Write([]byte("success"))

	if !w.seen.Claim(p.MsgID + "|" + p.FromUserName + "|" + fmt.Sprint(p.CreateTime)) {
		return
	}
	if p.MsgType == "event" {
		// system callbacks (FromUserName=sys) bypass the user allowlist
		if msg, ok := w.toEventMessage(p); ok {
			w.mu.Lock()
			pump := w.pump
			w.mu.Unlock()
			if pump != nil {
				go func() {
					select {
					case pump <- msg:
					case <-time.After(30 * time.Second):
					}
				}()
			}
		}
		return
	}
	if len(w.opts.AllowUsers) > 0 && !contains(w.opts.AllowUsers, p.FromUserName) {
		return
	}
	msg := w.toMessage(p)
	if msg.Text == "" && len(msg.Attachments) == 0 {
		return
	}
	// WeCom expects a fast ack; deliver asynchronously via the pump
	// registered in Start.
	w.mu.Lock()
	pump := w.pump
	w.mu.Unlock()
	if pump != nil {
		go func() {
			select {
			case pump <- msg:
			case <-time.After(30 * time.Second):
			}
		}()
	}
}

func (w *WeCom) toMessage(p cbPlain) channel.Message {
	chatType, chatID := "p2p", p.FromUserName
	if p.ChatID != "" {
		chatType, chatID = "group", p.ChatID
	}
	msg := channel.Message{
		ID:        p.MsgID,
		Channel:   w.name,
		ChatID:    chatID,
		ChatType:  chatType,
		UserID:    p.FromUserName,
		Text:      strings.TrimSpace(p.Content),
		Timestamp: time.Unix(p.CreateTime, 0),
	}
	switch p.MsgType {
	case "image":
		// Key set so media/get owns the fetch (pic_url is a short-lived
		// link); the URL stays as fallback if the download fails
		msg.Attachments = append(msg.Attachments, channel.Attachment{
			Type: "image", URL: p.PicURL, Name: p.MediaID, Key: p.MediaID,
		})
	case "voice":
		msg.Attachments = append(msg.Attachments, channel.Attachment{
			Type: "audio", Key: p.MediaID,
		})
	case "file":
		msg.Attachments = append(msg.Attachments, channel.Attachment{
			Type: "file", Key: p.MediaID,
		})
	}
	return msg
}

// toEventMessage maps WeCom chat-change callbacks (change_external_chat /
// chat.update) to synthetic member events. WeCom self-built apps have no
// "bot added to group" callback of their own — what the platform reports
// is the external-chat lifecycle — so these stay usage-log events; the
// feishu/dingtalk/telegram join-welcome does not apply here.
func (w *WeCom) toEventMessage(p cbPlain) (channel.Message, bool) {
	if p.Event != "change_external_chat" && p.Event != "chat.update" {
		return channel.Message{}, false
	}
	if p.ChatID == "" {
		return channel.Message{}, false
	}
	ev := "group_updated"
	switch p.ChangeType {
	case "create":
		ev = "group_created"
	case "dismiss":
		ev = "group_disbanded"
	case "update":
		switch p.UpdateDetail {
		case "add_member":
			ev = "member_added"
		case "del_member":
			ev = "member_removed"
		}
	}
	return channel.Message{
		ID:        fmt.Sprintf("evt-%d-%s", p.CreateTime, p.ChangeType),
		Channel:   w.name,
		ChatID:    p.ChatID,
		ChatType:  "group",
		UserID:    p.FromUserName,
		Event:     ev,
		Timestamp: time.Unix(p.CreateTime, 0),
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

// ---- inbound pump (set in Start) ----

func (w *WeCom) Start(ctx context.Context, out chan<- channel.Message) error {
	w.mu.Lock()
	w.pump = out
	w.mu.Unlock()
	<-ctx.Done()
	return nil
}

// ---- outbound ----

type Sender struct {
	w        *WeCom
	chatID   string
	chatType string
}

func (w *WeCom) SenderFor(msg channel.Message) (channel.Sender, error) {
	return &Sender{w: w, chatID: msg.ChatID, chatType: msg.ChatType}, nil
}

// Download implements channel.AttachmentDownloader: file/voice/image
// attachments arrive as media_id; media/get serves the bytes. Errors come
// back as HTTP 200 with a JSON body, so the content type decides which
// path we're on before anything is read as media.
func (w *WeCom) Download(ctx context.Context, att channel.Attachment) ([]byte, string, error) {
	if att.Key == "" {
		return nil, "", fmt.Errorf("wecom: attachment has no media id")
	}
	tok, err := w.accessToken(ctx)
	if err != nil {
		return nil, "", err
	}
	u := fmt.Sprintf("%s/cgi-bin/media/get?access_token=%s&media_id=%s",
		w.opts.APIBase, url.QueryEscape(tok), url.QueryEscape(att.Key))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		var res apiResult
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return nil, "", fmt.Errorf("wecom media/get: %d %s", res.ErrCode, res.ErrMsg)
	}
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("wecom media/get: %s", resp.Status)
	}
	data, err := channel.ReadCapped(resp.Body)
	if err != nil {
		return nil, "", err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		mime = http.DetectContentType(data)
	}
	return data, mime, nil
}

func (s *Sender) Send(ctx context.Context, text string, opts channel.SendOptions) (string, error) {
	tok, err := s.w.accessToken(ctx)
	if err != nil {
		return "", err
	}
	if s.chatType == "group" {
		// appchat/send addresses the group by chatid
		body := map[string]any{
			"chatid":  s.chatID,
			"msgtype": "text",
			"text":    map[string]string{"content": text},
			"safe":    0,
		}
		var res apiResult
		err := s.w.call(ctx, "/cgi-bin/appchat/send", tok, body, &res)
		return "", err
	}
	body := map[string]any{
		"touser":  s.chatID,
		"msgtype": "text",
		"agentid": s.w.opts.AgentID,
		"text":    map[string]string{"content": text},
		"safe":    0,
	}
	var res apiResult
	err = s.w.call(ctx, "/cgi-bin/message/send", tok, body, &res)
	return "", err
}

// Replace: WeCom has no message-edit API — unsupported by design; the
// runtime falls back to a single final send.
func (s *Sender) Replace(ctx context.Context, msgID, text string) error {
	return fmt.Errorf("wecom: message edit unsupported")
}

func (s *Sender) React(ctx context.Context, msgID, emoji string) error {
	return fmt.Errorf("wecom: reactions unsupported")
}

type apiResult struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func (w *WeCom) accessToken(ctx context.Context) (string, error) {
	w.mu.Lock()
	if w.accTok != "" && time.Now().Before(w.tokExp) {
		tok := w.accTok
		w.mu.Unlock()
		return tok, nil
	}
	w.mu.Unlock()

	var res struct {
		apiResult
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	u := fmt.Sprintf("%s/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		w.opts.APIBase, url.QueryEscape(w.opts.CorpID), url.QueryEscape(w.opts.Secret))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := w.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if res.ErrCode != 0 {
		return "", fmt.Errorf("wecom gettoken: %d %s", res.ErrCode, res.ErrMsg)
	}
	w.mu.Lock()
	w.accTok = res.AccessToken
	w.tokExp = time.Now().Add(time.Duration(res.ExpiresIn-120) * time.Second)
	w.mu.Unlock()
	return res.AccessToken, nil
}

func (w *WeCom) call(ctx context.Context, path, tok string, body any, out any) error {
	b, _ := json.Marshal(body)
	u := fmt.Sprintf("%s%s?access_token=%s", w.opts.APIBase, path, url.QueryEscape(tok))
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}
	if r, ok := out.(*apiResult); ok && r.ErrCode == 42001 {
		// token expired between cache check and call: refresh once
		w.mu.Lock()
		w.accTok = ""
		w.mu.Unlock()
	}
	if r, ok := out.(*apiResult); ok && r.ErrCode != 0 {
		return fmt.Errorf("wecom %s: %d %s", path, r.ErrCode, r.ErrMsg)
	}
	return nil
}
