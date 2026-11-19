// Small filesystem helpers for the agent runtime.
package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"

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
