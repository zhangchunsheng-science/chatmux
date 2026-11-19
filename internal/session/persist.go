// Persistence for the session store: snapshots to sessions.json under
// DataDir so conversations (and their model/system overrides) survive
// restarts. CLI resume ids persist alongside in cli-sessions.json —
// see cliagent.Runner.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhangchunsheng-science/chatmux/internal/provider"
)

// persistedSession is the on-disk form of one session.
type persistedSession struct {
	History []provider.Msg `json:"history"`
	System  string         `json:"system,omitempty"`
	Model   string         `json:"model,omitempty"`
	Title   string         `json:"title,omitempty"`
	Agent   string         `json:"agent,omitempty"`
}

// Save snapshots all sessions atomically (tmp + rename).
func (s *Store) Save(dataDir string) error {
	if dataDir == "" {
		dataDir = "data"
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	s.mu.Lock()
	out := make(map[string]persistedSession, len(s.byKey))
	for k, sess := range s.byKey {
		// skip background-job pseudo sessions (chat|jobN keys)
		if strings.Contains(k.ChatID, "|job") {
			continue
		}
		out[k.String()] = persistedSession{History: sess.History, System: sess.System, Model: sess.Model, Title: sess.Title, Agent: sess.Agent}
	}
	s.mu.Unlock()
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	p := filepath.Join(dataDir, "sessions.json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoadSessions restores sessions saved by Save. Unknown/corrupt file is
// not an error: the store just starts empty.
func LoadSessions(dataDir string) *Store {
	s := NewStore()
	if dataDir == "" {
		dataDir = "data"
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "sessions.json"))
	if err != nil {
		return s
	}
	var in map[string]persistedSession
	if json.Unmarshal(b, &in) != nil {
		return s
	}
	for anchor, ps := range in {
		k, ok := parseAnchor(anchor)
		if !ok {
			continue
		}
		s.byKey[k] = &Session{Key: k, History: ps.History, System: ps.System, Model: ps.Model, Title: ps.Title, Agent: ps.Agent}
	}
	return s
}

// parseAnchor reverses Key.String: channel|chat[|thread].
func parseAnchor(anchor string) (Key, bool) {
	parts := splitUnescaped(anchor)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Key{}, false
	}
	k := Key{Channel: parts[0], ChatID: parts[1]}
	if len(parts) == 3 {
		k.ThreadID = parts[2]
	}
	return k, true
}

// splitUnescaped splits on '|' — chat ids may legitimately contain '|'
// only in the background-job pseudo keys we generate ourselves, so a
// plain split is fine here (job keys restore as their own sessions).
func splitUnescaped(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
