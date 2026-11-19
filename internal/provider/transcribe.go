// Voice transcription against an OpenAI-compatible audio/transcriptions
// endpoint (whisper-1 and friends; any self-hosted compatible gateway
// works via BaseURL).
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TranscribeArgs describes one transcription request.
type TranscribeArgs struct {
	Model   string // whisper-1
	APIKey  string
	BaseURL string
}

// TranscribeAudio POSTs an audio file to {base}/audio/transcriptions.
func TranscribeAudio(ctx context.Context, client *http.Client, path string, args TranscribeArgs) (string, error) {
	if args.Model == "" {
		args.Model = "whisper-1"
	}
	if args.BaseURL == "" {
		args.BaseURL = "https://api.openai.com/v1"
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.CloseWithError(nil)
		_ = mw.WriteField("model", args.Model)
		part, err := mw.CreateFormFile("file", filepath.Base(path))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, f); err != nil {
			pw.CloseWithError(err)
			return
		}
		_ = mw.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(args.BaseURL, "/")+"/audio/transcriptions", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+args.APIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("transcribe: %s %s", resp.Status, truncateStr(string(b), 300))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := jsonUnmarshal(b, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Text), nil
}
