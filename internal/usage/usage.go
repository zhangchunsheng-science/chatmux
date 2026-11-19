// Package usage records per-turn token usage to dataDir/usage.jsonl —
// one JSON line per completed model call or CLI-agent turn.
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// Agg is a per-agent usage summary.
type Agg struct {
	Agent        string `json:"agent"`
	Calls        int    `json:"calls"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	OutputChars  int64  `json:"outputChars"`
	FeedbackUp   int    `json:"feedbackUp"`
	FeedbackDown int    `json:"feedbackDown"`
}

// Summarize aggregates usage.jsonl by agent. feedback rows (Mode
// "feedback:up"/"feedback:down") count into the tally.
func Summarize(dataDir string) ([]Agg, error) {
	return SummarizeSince(dataDir, time.Time{})
}

// SummarizeSince aggregates like Summarize but only rows at/after since
// (zero = all rows). Malformed or empty timestamps count as "no filter
// match" — old rows without timestamps are only kept for a zero since.
func SummarizeSince(dataDir string, since time.Time) ([]Agg, error) {
	if dataDir == "" {
		dataDir = "data"
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "usage.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	byAgent := map[string]*Agg{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if !since.IsZero() {
			ts, err := time.Parse(time.RFC3339, rec.Time)
			if err != nil || ts.Before(since) {
				continue
			}
		}
		// feedback rows are logged with Agent="feedback" and Channel=<instance>
		if strings.HasPrefix(rec.Mode, "feedback:") {
			name := rec.Agent
			if name == "" {
				name = rec.Channel
			}
			a := byAgent[name]
			if a == nil {
				a = &Agg{Agent: name}
				byAgent[name] = a
			}
			if strings.HasSuffix(rec.Mode, "up") {
				a.FeedbackUp++
			} else {
				a.FeedbackDown++
			}
			continue
		}
		a := byAgent[rec.Agent]
		if a == nil {
			a = &Agg{Agent: rec.Agent}
			byAgent[rec.Agent] = a
		}
		a.Calls++
		a.InputTokens += int64(rec.InputTokens)
		a.OutputTokens += int64(rec.OutputTokens)
		a.OutputChars += int64(rec.OutputChars)
	}
	out := make([]Agg, 0, len(byAgent))
	for _, a := range byAgent {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Calls > out[j].Calls })
	return out, nil
}
