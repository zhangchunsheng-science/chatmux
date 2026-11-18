// Small filesystem helpers for the agent runtime.
package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
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
