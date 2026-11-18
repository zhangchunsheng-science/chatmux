package feishu

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

// download fetches a binary resource (im/v1/images/:key) and returns the
// raw bytes plus the Content-Type.
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
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("feishu %s: %s %s", path, resp.Status, truncate(string(b), 300))
	}
	return b, resp.Header.Get("Content-Type"), nil
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
