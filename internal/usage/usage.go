// Package usage records per-turn token usage to dataDir/usage.jsonl —
// one JSON line per completed model call or CLI-agent turn.
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one usage line.
type Record struct {
	Time         string `json:"time"`
	Agent        string `json:"agent"`
	Mode         string `json:"mode"` // "api" | "cli"
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	Channel      string `json:"channel,omitempty"`
	ChatID       string `json:"chatId,omitempty"`
	InputTokens  int    `json:"inputTokens,omitempty"`
	OutputTokens int    `json:"outputTokens,omitempty"`
	OutputChars  int    `json:"outputChars,omitempty"`
	DurationMs   int64  `json:"durationMs,omitempty"`
}

// Logger appends records to <dataDir>/usage.jsonl.
type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// New opens (append) the usage log under dataDir.
func New(dataDir string) *Logger {
	if dataDir == "" {
		dataDir = "data"
	}
	_ = os.MkdirAll(dataDir, 0o755)
	f, err := os.OpenFile(filepath.Join(dataDir, "usage.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return &Logger{} // disabled; Log is a no-op
	}
	return &Logger{f: f}
}

// Log appends one record with a timestamp.
func (l *Logger) Log(rec Record) {
	if l == nil || l.f == nil {
		return
	}
	rec.Time = time.Now().Format(time.RFC3339)
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(append(b, '\n'))
}

// Close flushes and closes the log.
func (l *Logger) Close() {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Close()
}
