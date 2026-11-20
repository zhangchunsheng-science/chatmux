package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

// api is a minimal Feishu OpenAPI REST client: tenant_access_token cache
// + the handful of endpoints chatmux needs. Keeps behavior transparent
// instead of leaning on the generated SDK surface.
type api struct {
	appID     string
	appSecret string
	base      string // https://open.feishu.cn or https://open.larksuite.com
	client    *http.Client

	mu     sync.Mutex
	token  string
	tokExp time.Time

	mu2    sync.Mutex
	botOID string // this app's bot open_id, learned at boot
}

func (a *api) tenantToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && time.Now().Before(a.tokExp) {
		t := a.token
		a.mu.Unlock()
		return t, nil
	}
	a.mu.Unlock()

	body, _ := json.Marshal(map[string]string{
		"app_id": a.appID, "app_secret": a.appSecret,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		a.base+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Code != 0 || out.TenantAccessToken == "" {
		return "", fmt.Errorf("feishu token: code=%d msg=%s", out.Code, out.Msg)
	}
	a.mu.Lock()
	a.token = out.TenantAccessToken
	a.tokExp = time.Now().Add(time.Duration(out.Expire-300) * time.Second)
	a.mu.Unlock()
	return out.TenantAccessToken, nil
}

func (a *api) botOpenID(ctx context.Context) string {
	a.mu2.Lock()
	cached := a.botOID
	a.mu2.Unlock()
	if cached != "" {
		return cached
	}
	tok, err := a.tenantToken(ctx)
	if err != nil {
		return ""
	}
	var out struct {
		Code int `json:"code"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := a.get(ctx, "/open-apis/bot/v3/info", tok, &out); err != nil || out.Bot.OpenID == "" {
		return ""
	}
	a.mu2.Lock()
	a.botOID = out.Bot.OpenID
	a.mu2.Unlock()
	return out.Bot.OpenID
}

func (a *api) get(ctx context.Context, path, tok string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", a.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	return a.do(req, out)
}

func (a *api) post(ctx context.Context, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", a.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return a.do(req, out)
}

func (a *api) patch(ctx context.Context, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "PATCH", a.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return a.do(req, out)
}

func (a *api) do(req *http.Request, out any) error {
	tok, err := a.tenantToken(req.Context())
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("feishu %s: %s %s", req.URL.Path, resp.Status, truncate(string(b), 300))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// uploadFile posts a local file to im/v1/images (image_type=message) or
// im/v1/files (file_type=stream) and returns the resource key.
func (a *api) uploadFile(ctx context.Context, path string, isImage bool) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	endpoint, fileField, fileType := "/open-apis/im/v1/files", "file", "stream"
	if isImage {
		endpoint, fileField, fileType = "/open-apis/im/v1/images", "image", "message"
	}
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("image_type", fileType) // images want image_type
	_ = w.WriteField("file_type", fileType)  // files want file_type
	part, err := w.CreateFormFile(fileField, filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	_ = w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", a.base+endpoint+"?user_id_type=open_id", &b)
	if err != nil {
		return "", err
	}
	tok, err := a.tenantToken(ctx)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("feishu upload %s: %s %s", endpoint, resp.Status, truncate(string(body), 300))
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ImageKey string `json:"image_key"`
			FileKey  string `json:"file_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("feishu upload: code=%d msg=%s", out.Code, out.Msg)
	}
	if out.Data.ImageKey != "" {
		return out.Data.ImageKey, nil
	}
	return out.Data.FileKey, nil
}

// download fetches a binary resource (im/v1/images/:key) and returns the
// raw bytes plus the Content-Type. Payloads over channel.MaxAttachmentBytes
// fail instead of being silently truncated.
func (a *api) download(ctx context.Context, path string) ([]byte, string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", a.base+path, nil)
	tok, err := a.tenantToken(ctx)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, "", fmt.Errorf("feishu %s: %s %s", path, resp.Status, truncate(string(b), 300))
	}
	b, err := channel.ReadCapped(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// getMessageText fetches one message's body text (for reply quotes).
func (a *api) getMessageText(ctx context.Context, messageID string) (string, string, error) {
	var out struct {
		Items []struct {
			MsgType string `json:"msg_type"`
			Body    struct {
				Content string `json:"content"`
			} `json:"body"`
			Sender struct {
				ID   string `json:"id"`
				Type string `json:"sender_type"`
			} `json:"sender"`
		} `json:"items"`
	}
	if err := a.get(ctx, "/open-apis/im/v1/messages/"+messageID, "", &out); err != nil {
		return "", "", err
	}
	if len(out.Items) == 0 {
		return "", "", fmt.Errorf("feishu: message %s not found", messageID)
	}
	it := out.Items[0]
	text, _ := parseContent(it.MsgType, it.Body.Content)
	return text, it.Sender.ID, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---- messaging endpoints ----

func (a *api) sendMessage(ctx context.Context, receiveIDType, receiveID, msgType, content string, replyTo string) (string, error) {
	var path string
	if replyTo != "" {
		path = fmt.Sprintf("/open-apis/im/v1/messages/%s/reply", replyTo)
	} else {
		path = "/open-apis/im/v1/messages?receive_id_type=" + receiveIDType
	}
	body := map[string]string{
		"receive_id": receiveID,
		"msg_type":   msgType,
		"content":    content,
	}
	if replyTo != "" {
		body = map[string]string{"msg_type": msgType, "content": content}
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := a.post(ctx, path, body, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("feishu send: code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Data.MessageID, nil
}

// patchMessage replaces an interactive card's content in place — the
// streaming-update path (im/v1/messages/:id PATCH only supports cards).
func (a *api) patchMessage(ctx context.Context, messageID, cardJSON string) error {
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := a.patch(ctx, "/open-apis/im/v1/messages/"+messageID, map[string]string{
		"content": cardJSON,
	}, &out); err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("feishu patch: code=%d msg=%s", out.Code, out.Msg)
	}
	return nil
}

func (a *api) addReaction(ctx context.Context, messageID, emojiType string) error {
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := a.post(ctx, "/open-apis/im/v1/messages/"+messageID+"/reactions", map[string]any{
		"reaction_type": map[string]string{"emoji_type": emojiType},
	}, &out); err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("feishu reaction: code=%d msg=%s", out.Code, out.Msg)
	}
	return nil
}

// textContent builds the JSON payload of a text message.
func textContent(text string, mentions ...string) string {
	// escape into {"text": "..."} with optional <at user_id="..."> tags
	b, _ := json.Marshal(map[string]string{"text": withMentions(text, mentions...)})
	return string(b)
}

func withMentions(text string, mentions ...string) string {
	if len(mentions) == 0 {
		return text
	}
	var b strings.Builder
	for _, m := range mentions {
		b.WriteString(`<at user_id="`)
		b.WriteString(m)
		b.WriteString(`"></at> `)
	}
	b.WriteString(text)
	return b.String()
}

// cardContent builds a simple interactive card that renders markdown-ish
// text — used for stream-in-place updates.
func cardContent(title, text string) string {
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"elements": []any{
			map[string]any{
				"tag":     "markdown",
				"content": text,
			},
		},
	}
	if title != "" {
		card["header"] = map[string]any{
			"title":    map[string]string{"tag": "plain_text", "content": title},
			"template": "blue",
		}
	}
	b, _ := json.Marshal(card)
	return string(b)
}

// feedbackCard builds a final-answer card with 👍/👎 feedback buttons.
// lark_md <at> tags fold the mention into the text.
func feedbackCard(text string) string {
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"elements": []any{
			map[string]any{"tag": "markdown", "content": text},
			map[string]any{
				"tag": "action",
				"actions": []any{
					map[string]any{
						"tag":   "button",
						"text":  map[string]string{"tag": "plain_text", "content": "👍"},
						"type":  "primary",
						"value": map[string]string{"chatmux_feedback": "up"},
					},
					map[string]any{
						"tag":   "button",
						"text":  map[string]string{"tag": "plain_text", "content": "👎"},
						"type":  "danger",
						"value": map[string]string{"chatmux_feedback": "down"},
					},
				},
			},
			map[string]any{"tag": "hr"},
		},
	}
	b, _ := json.Marshal(card)
	return string(b)
}

// formCard builds a card JSON 2.0 form: labeled inputs + a form_submit
// button carrying {"chatmux_form": formName} (the routing token).
// Submitted values come back on the card-action callback in
// Action.FormValue.
func formCard(title, formName, desc string, fields []channel.FormField) string {
	elements := []any{}
	if desc != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": desc})
	}
	for _, f := range fields {
		input := map[string]any{"tag": "input", "name": f.Name}
		if f.Label != "" {
			input["label"] = map[string]string{"tag": "plain_text", "content": f.Label}
		}
		if f.Required != nil && *f.Required {
			input["required"] = true
		}
		ph := f.Placeholder
		if ph == "" {
			ph = f.Label
		}
		switch f.Type {
		case "select":
			opts := make([]any, 0, len(f.Options))
			for _, o := range f.Options {
				opts = append(opts, map[string]any{"text": map[string]string{"tag": "plain_text", "content": o}})
			}
			input["element"] = map[string]any{
				"tag":         "select_static",
				"placeholder": map[string]string{"tag": "plain_text", "content": ph},
				"options":     opts,
			}
		case "textarea":
			input["element"] = map[string]any{
				"tag":         "plain_text_input",
				"multiline":   true,
				"placeholder": map[string]string{"tag": "plain_text", "content": ph},
			}
		default: // input
			input["element"] = map[string]any{
				"tag":         "plain_text_input",
				"placeholder": map[string]string{"tag": "plain_text", "content": ph},
			}
		}
		elements = append(elements, input)
	}
	elements = append(elements, map[string]any{
		"tag":          "button",
		"action_type":  "form_submit",
		"name":         "chatmux_form_submit",
		"text":         map[string]string{"tag": "plain_text", "content": "提交"},
		"button_style": "primary",
		"form_value":   map[string]string{"chatmux_form": formName},
	})
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{
			"title":    map[string]string{"tag": "plain_text", "content": title},
			"template": "blue",
		},
		"body": map[string]any{"elements": elements},
	}
	b, _ := json.Marshal(card)
	return string(b)
}
