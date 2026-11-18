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

// Anthropic is the Claude messages API provider (streaming SSE).
type Anthropic struct {
	APIKey  string
	BaseURL string // default https://api.anthropic.com
	Model   string
	Client  *http.Client
}

func (a *Anthropic) Name() string { return "anthropic" }

type anImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url,omitempty"` // Anthropic supports url source too
}

type anContentBlock struct {
	Type  string  `json:"type"`
	Text  string  `json:"text,omitempty"`
	Source *anImage `json:"source,omitempty"`
}

func (a *Anthropic) buildMessages(req Request) [][]anContentBlock {
	var out [][]anContentBlock
	for _, m := range req.Messages {
		var blocks []anContentBlock
		if m.Content != "" || len(m.Images) > 0 {
			if m.Content != "" {
				blocks = append(blocks, anContentBlock{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				if mt, data := anImageData(img); data != "" {
					blocks = append(blocks, anContentBlock{Type: "image", Source: &anImage{
						Type: "base64", MediaType: mt, Data: data,
					}})
				} else if strings.HasPrefix(img, "http") {
					blocks = append(blocks, anContentBlock{Type: "image", Source: &anImage{
						Type: "url", URL: img,
					}})
				}
			}
		}
		if blocks == nil {
			blocks = []anContentBlock{{Type: "text", Text: "(empty)"}}
		}
		out = append(out, blocks)
	}
	return out
}

func anImageData(p string) (mime, b64 string) {
	if strings.HasPrefix(p, "http") {
		return "", ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", ""
	}
	mime = "image/png"
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".webp":
		mime = "image/webp"
	case ".gif":
		mime = "image/gif"
	}
	return mime, base64.StdEncoding.EncodeToString(b)
}

func (a *Anthropic) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	model := req.Model
	if model == "" {
		model = a.Model
	}
	if model == "" {
		model = "claude-sonnet-4-5"
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	key := req.APIKey
	if key == "" {
		key = a.APIKey
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	temp := req.Temperature
	if temp == 0 {
		temp = 0.7
	}

	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"temperature": temp,
		"stream":     true,
		"system":     req.System,
		"messages":   a.buildMessages(req),
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", key)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	client := a.Client
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
		return nil, fmt.Errorf("anthropic: %s %s", resp.Status, string(b))
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
			var ev struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
				Message struct {
					Usage struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				} `json:"message"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			switch ev.Type {
			case "content_block_delta":
				if ev.Delta.Text != "" {
					ch <- Delta{Text: ev.Delta.Text}
				}
			case "message_start":
				if ev.Message.Usage.InputTokens > 0 {
					ch <- Delta{InputTokens: ev.Message.Usage.InputTokens}
				}
			case "message_delta":
				if ev.Usage.OutputTokens > 0 {
					ch <- Delta{OutputTokens: ev.Usage.OutputTokens}
				}
			case "error":
				ch <- Delta{Err: fmt.Errorf("anthropic: %s", ev.Error.Message)}
				return
			case "message_stop":
				ch <- Delta{Done: true}
				return
			}
		}
		if err := sc.Err(); err != nil {
			ch <- Delta{Err: err}
		}
	}()
	return ch, nil
}
