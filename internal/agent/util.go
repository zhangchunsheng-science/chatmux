// Small filesystem helpers for the agent runtime.
package agent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/provider"
)

func hashOf(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func extOf(mime string) string {
	switch mime {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/bmp":
		return "bmp"
	}
	return "bin"
}

func saveFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// maxHistoryChars bounds the history replayed to the model each turn: pasted
// documents can make even a handful of turns exceed the model's context, and
// the per-pair count cap alone doesn't prevent that. Oldest exchanges go first.
const maxHistoryChars = 80_000

// trimToBudget drops the oldest messages until the total content size fits
// the budget, always keeping at least one user/assistant exchange.
func trimToBudget(hist []provider.Msg, budget int) []provider.Msg {
	total := 0
	for _, m := range hist {
		total += len(m.Content)
	}
	for total > budget && len(hist) > 2 {
		total -= len(hist[0].Content)
		hist = hist[1:]
	}
	return hist
}

// errStr renders a nil-able error for record fields.
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// fmtDur renders milliseconds compactly for /insight lines.
func fmtDur(ms int64) string {
	switch {
	case ms < 1000:
		return strconv.FormatInt(ms, 10) + "ms"
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm%ds", ms/60_000, (ms%60_000)/1000)
	}
}

// Attachment size caps live in the channel package
// (channel.MaxAttachmentBytes): downloaders reject over-cap payloads at the
// HTTP layer, and the runtime-side checks below are belt-and-braces for
// channels that do not stream-cap yet.

// fetchURL GETs a public attachment URL (no auth headers) under the
// shared size cap — used for channels whose audio arrives as a plain
// CDN link.
func fetchURL(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("fetch %s: %s", url, resp.Status)
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

// streamRetryDelay is the breath between a failed stream start and its
// single retry.
const streamRetryDelay = 2 * time.Second

// streamWithRetry starts a model stream, retrying once on a start
// failure: nothing has been produced yet, so the retry is invisible to
// the user and catches transient blips (connection resets, brief 5xx).
func streamWithRetry(ctx context.Context, p provider.Provider, req provider.Request) (<-chan provider.Delta, error) {
	ch, err := p.Stream(ctx, req)
	if err == nil || ctx.Err() != nil {
		return ch, err
	}
	log.Printf("model stream start failed (%v) — retrying once", err)
	timer := time.NewTimer(streamRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	return p.Stream(ctx, req)
}
