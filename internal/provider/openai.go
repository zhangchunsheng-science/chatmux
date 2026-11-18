package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OpenAI is a chat-completions provider speaking the OpenAI wire format
// (chat/completions with stream=true). Works with OpenAI, DeepSeek,
// Moonshot, vLLM, one-api and every other compatible gateway via BaseURL.
type OpenAI struct {
	APIKey  string
	BaseURL string // default https://api.openai.com/v1
	Model   string // default model when request doesn't set one
	Client  *http.Client
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) baseURL() string {
	if o.BaseURL != "" {
		return strings.TrimRight(o.BaseURL, "/")
	}
	return "https://api.openai.com/v1"
}

func (o *OpenAI) apiKey(req Request) string {
	if req.APIKey != "" {
		return req.APIKey
	}
	return o.APIKey
}

type oaMsgContent struct {
	Type     string      `json:"type,omitempty"`
	Text     string      `json:"text,omitempty"`
	ImageURL *oaImageURL `json:"image_url,omitempty"`
}

type oaImageURL struct {
	URL string `json:"url"`
}

type oaMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (o *OpenAI) buildMessages(req Request) []oaMessage {
	var out []oaMessage
	if req.System != "" {
		out = append(out, oaMessage{Role: "system", Content: mustJSON(req.System)})
	}
	for _, m := range req.Messages {
		if len(m.Images) == 0 {
			out = append(out, oaMessage{Role: string(m.Role), Content: mustJSON(m.Content)})
			continue
		}
		parts := []oaMsgContent{{Type: "text", Text: m.Content}}
		for _, img := range m.Images {
			if u := dataURLFor(img); u != "" {
				parts = append(parts, oaMsgContent{Type: "image_url", ImageURL: &oaImageURL{URL: u}})
			}
		}
		out = append(out, oaMessage{Role: string(m.Role), Content: mustJSON(parts)})
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// dataURLFor turns a local image path into a data: URL; http(s) URLs pass
// through as-is.
func dataURLFor(p string) string {
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return p // let the API complain
	}
	mime := "image/png"
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".webp":
		mime = "image/webp"
	case ".gif":
		mime = "image/gif"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

type oaStreamReq struct {
	Model     string          `json:"model"`
	Messages  []oaMessage     `json:"messages"`
	Stream    bool            `json:"stream"`
	Options   oaOptions       `json:"-"`
	StreamOpt oaStreamOptions `json:"stream_options"`
}

type oaOptions struct {
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
}

type oaStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

func (o *OpenAI) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	model := req.Model
	if model == "" {
		model = o.Model
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	temp := req.Temperature
	if temp == 0 {
		temp = 0.7
	}
	body, _ := json.Marshal(map[string]any{
		"model":          model,
		"messages":       o.buildMessages(req),
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
		"max_tokens":     req.MaxTokens,
		"temperature":    temp,
	})

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		o.baseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey(req))

	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, fmt.Errorf("openai: %s %s", resp.Status, string(b))
	}

	ch := make(chan Delta, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(line[5:])
			if string(data) == "[DONE]" {
				ch <- Delta{Done: true}
				return
			}
			var ev struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			if ev.Usage != nil {
				ch <- Delta{InputTokens: ev.Usage.PromptTokens, OutputTokens: ev.Usage.CompletionTokens}
			}
			for _, c := range ev.Choices {
				if c.Delta.Content != "" {
					ch <- Delta{Text: c.Delta.Content}
				}
			}
		}
		if err := sc.Err(); err != nil {
			ch <- Delta{Err: err}
		}
	}()
	return ch, nil
}
