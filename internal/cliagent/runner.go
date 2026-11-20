// Package cliagent drives external coding-CLI agents (claude, codex, or
// any line-oriented binary) headlessly — chatmux's counterpart to
// botmux's worker/PTY machinery, implemented via each CLI's native
// non-interactive streaming mode instead of screen scraping.
package cliagent

import (
	"github.com/zhangchunsheng-science/chatmux/internal/atomicfile"

	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event is one incremental output from a running CLI agent.
type Event struct {
	Type string `json:"type"` // "text" | "tool" | "result" | "error"
	Text string `json:"text"`
	// SessionID is set on "result": the CLI's own session id to resume.
	SessionID string `json:"sessionId"`
	Err       error  `json:"-"`
}

// cliSess is one stored resume id plus its last-use stamp — the stamp
// drives eviction when the map grows past maxCliSessions.
type cliSess struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// maxCliSessions caps the resume-id table: entries are tiny, but an
// anchor per chat ever seen would otherwise grow without end.
const maxCliSessions = 2000

// Runner spawns one CLI process per turn (headless CLIs own their own
// conversational memory via resume ids; chatmux tracks the ids).
type Runner struct {
	mu       sync.Mutex
	sessions map[string]cliSess // chatmux anchor key → CLI session id
	persist  string             // cli-sessions.json path, "" = memory only
}

// NewRunner optionally restores resume ids persisted at dataDir. Reads
// both the current {id, at} format and the legacy plain-string one.
func NewRunner(dataDir ...string) *Runner {
	r := &Runner{sessions: map[string]cliSess{}}
	if len(dataDir) > 0 && dataDir[0] != "" {
		r.persist = filepath.Join(dataDir[0], "cli-sessions.json")
		if b, err := os.ReadFile(r.persist); err == nil {
			var m map[string]cliSess
			if json.Unmarshal(b, &m) == nil {
				r.sessions = m
			} else {
				var legacy map[string]string
				if json.Unmarshal(b, &legacy) == nil {
					for k, id := range legacy {
						r.sessions[k] = cliSess{ID: id}
					}
				}
			}
		}
	}
	return r
}

// save snapshots resume ids (atomic tmp+rename, best effort), evicting
// the stalest entries past the cap.
func (r *Runner) save() {
	if r.persist == "" {
		return
	}
	r.mu.Lock()
	if len(r.sessions) > maxCliSessions {
		type stamp struct {
			anchor string
			at     time.Time
		}
		stamps := make([]stamp, 0, len(r.sessions))
		for k, s := range r.sessions {
			stamps = append(stamps, stamp{k, s.At})
		}
		sort.Slice(stamps, func(i, j int) bool { return stamps[i].at.Before(stamps[j].at) })
		for _, st := range stamps[:len(stamps)-maxCliSessions] {
			delete(r.sessions, st.anchor)
		}
	}
	b, err := json.Marshal(r.sessions)
	r.mu.Unlock()
	if err != nil {
		return
	}
	_ = atomicfile.Write(r.persist, b, 0o600)
}

// ResumeID returns the stored CLI session id for an anchor.
func (r *Runner) ResumeID(anchor string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[anchor].ID
}

func (r *Runner) setResumeID(anchor, id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	r.sessions[anchor] = cliSess{ID: id, At: time.Now()}
	r.mu.Unlock()
	r.save()
}

// Reset forgets the CLI session for an anchor (next turn starts fresh).
func (r *Runner) Reset(anchor string) {
	r.mu.Lock()
	delete(r.sessions, anchor)
	r.mu.Unlock()
	r.save()
}

// CheckConfig validates a CLI config's invocation shape (type, binary)
// without running anything — used by `chatmux check`.
func CheckConfig(cfg Config) error {
	_, _, _, err := buildInvocation(cfg, "", "ping")
	return err
}

// Config describes how to invoke one CLI.
type Config struct {
	Type       string `json:"type"`  // claude | codex | gemini | qwen | opencode | aider | generic
	Bin        string `json:"bin"`   // executable; defaults per type
	Model      string `json:"model"` // passed through when supported
	WorkingDir string `json:"workingDir"`
	// Sandbox isolates the CLI process (mode "docker").
	Sandbox *SandboxConfig `json:"sandbox,omitempty"`
	// generic type:
	PromptArg  bool   `json:"promptArg"`  // pass prompt as argv (else stdin)
	ResumeFlag string `json:"resumeFlag"` // e.g. "--resume"
}

// SandboxConfig isolates the CLI in a container so a rogue agent can't
// touch the host. mode "docker": the CLI runs as
// docker run --rm -i [--network none] [-v workdir:/work] <image> <bin>...
type SandboxConfig struct {
	Mode    string `json:"mode"`              // "docker" ("" = off)
	Image   string `json:"image,omitempty"`   // default node:20-slim
	Network string `json:"network,omitempty"` // default "none"
}

// buildInvocation derives the executable + argv for one turn.
func buildInvocation(cfg Config, resumeID, prompt string) (bin string, args []string, promptViaStdin bool, err error) {
	promptViaStdin = true
	switch cfg.Type {
	case "claude", "":
		if bin == "" {
			bin = "claude"
		}
		args = []string{"-p", "--output-format", "stream-json", "--verbose"}
		if resumeID != "" {
			args = append(args, "--resume", resumeID)
		}
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
	case "codex":
		if bin == "" {
			bin = "codex"
		}
		args = []string{"exec", "--json"}
		if resumeID != "" {
			args = append(args, "resume", resumeID)
		}
		if cfg.Model != "" {
			args = append(args, "-m", cfg.Model)
		}
	case "gemini":
		// gemini CLI headless: prompt as argv, plain text on stdout.
		// No native resume; each turn is fresh (skills keep context).
		if bin == "" {
			bin = "gemini"
		}
		promptViaStdin = false
		args = []string{"-p", prompt}
		if cfg.Model != "" {
			args = append(args, "-m", cfg.Model)
		}
	case "qwen":
		// Qwen Code CLI (gemini-cli fork): same headless shape.
		if bin == "" {
			bin = "qwen"
		}
		promptViaStdin = false
		args = []string{"-p", prompt}
		if cfg.Model != "" {
			args = append(args, "-m", cfg.Model)
		}
	case "opencode":
		// opencode headless: `opencode run <prompt>`, plain text out.
		if bin == "" {
			bin = "opencode"
		}
		promptViaStdin = false
		args = []string{"run", prompt}
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
	case "aider":
		// aider: one message per run, auto-accept, no git churn.
		if bin == "" {
			bin = "aider"
		}
		promptViaStdin = false
		args = []string{"--yes-always", "--no-auto-commits", "--message", prompt}
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
	case "generic":
		if cfg.Bin == "" {
			return "", nil, false, fmt.Errorf("cliagent: generic runner needs bin")
		}
		bin = cfg.Bin
		promptViaStdin = !cfg.PromptArg
		if cfg.PromptArg {
			args = append(args, prompt)
		}
		if resumeID != "" && cfg.ResumeFlag != "" {
			args = append(args, cfg.ResumeFlag, resumeID)
		}
	default:
		return "", nil, false, fmt.Errorf("cliagent: unknown type %q", cfg.Type)
	}
	// sandbox wrap: bin+args run inside the container instead
	if cfg.Sandbox != nil && cfg.Sandbox.Mode == "docker" {
		bin, args, err = wrapDocker(cfg, bin, args)
	}
	return bin, args, promptViaStdin, err
}

// wrapDocker rewrites the invocation as docker run --rm -i <image> bin args…
func wrapDocker(cfg Config, bin string, args []string) (string, []string, error) {
	sc := cfg.Sandbox
	image := sc.Image
	if image == "" {
		image = "node:20-slim"
	}
	net := sc.Network
	if net == "" {
		net = "none"
	}
	dargs := []string{"run", "--rm", "-i", "--network", net}
	if cfg.WorkingDir != "" {
		abs, err := filepath.Abs(cfg.WorkingDir)
		if err != nil {
			return "", nil, err
		}
		dargs = append(dargs, "-v", abs+":/work", "-w", "/work")
	}
	dargs = append(dargs, image, bin)
	return "docker", append(dargs, args...), nil
}

// Run executes one turn: builds the command line, streams events.
// anchor keys the resume memory. Cancelling ctx kills the process.
func (r *Runner) Run(ctx context.Context, cfg Config, anchor, prompt string) (<-chan Event, error) {
	if cfg.Type == "" {
		cfg.Type = "claude"
	}
	bin, args, promptViaStdin, err := buildInvocation(cfg, r.ResumeID(anchor), prompt)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	if cfg.WorkingDir != "" {
		cmd.Dir = cfg.WorkingDir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cliagent start %s: %w", bin, err)
	}
	if promptViaStdin {
		go func() {
			_, _ = io.WriteString(stdin, prompt)
			_ = stdin.Close()
		}()
	}

	events := make(chan Event, 64)
	go func() {
		defer close(events)
		go func() { _ = cmd.Wait() }()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 || line[0] != '{' {
				if strings.TrimSpace(string(line)) != "" {
					events <- Event{Type: "text", Text: string(line) + "\n"}
				}
				continue
			}
			for _, ev := range parseJSONLine(cfg.Type, line) {
				if ev.Type == "result" {
					r.setResumeID(anchor, ev.SessionID)
				}
				events <- ev
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			events <- Event{Type: "error", Err: err}
		}
	}()
	return events, nil
}

// parseJSONLine turns one stream-json record into chatmux events.
func parseJSONLine(cliType string, line []byte) []Event {
	var raw map[string]any
	if json.Unmarshal(line, &raw) != nil {
		return nil
	}
	switch cliType {
	case "claude":
		return parseClaudeEvent(raw)
	case "codex":
		return parseCodexEvent(raw)
	}
	// generic jsonl: emit text fields as-is
	if t, ok := raw["text"].(string); ok && t != "" {
		return []Event{{Type: "text", Text: t}}
	}
	if t, ok := raw["message"].(string); ok && t != "" {
		return []Event{{Type: "text", Text: t}}
	}
	return nil
}

// claude -p --output-format stream-json emits records with type:
// system | assistant | user | result. Assistant records carry message
// blocks; the final result carries result + session_id.
func parseClaudeEvent(raw map[string]any) []Event {
	t, _ := raw["type"].(string)
	var out []Event
	switch t {
	case "assistant":
		msg, _ := raw["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, block := range content {
			b, _ := block.(map[string]any)
			switch b["type"] {
			case "text":
				if s, ok := b["text"].(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, Event{Type: "text", Text: s})
				}
			case "tool_use":
				name, _ := b["name"].(string)
				out = append(out, Event{Type: "tool", Text: fmt.Sprintf("🔧 %s", name)})
			}
		}
	case "result":
		sid, _ := raw["session_id"].(string)
		res, _ := raw["result"].(string)
		out = append(out, Event{Type: "result", Text: res, SessionID: sid})
	}
	return out
}

// codex exec --json emits {msg:{type:...}} records.
func parseCodexEvent(raw map[string]any) []Event {
	msg, _ := raw["msg"].(map[string]any)
	if msg == nil {
		return nil
	}
	switch msg["type"] {
	case "agent_message", "agent_message_delta":
		if s, ok := msg["message"].(string); ok && s != "" {
			return []Event{{Type: "text", Text: s}}
		}
	case "token_count":
		return nil
	case "task_complete":
		sid, _ := raw["session_id"].(string)
		if sid == "" {
			sid, _ = msg["session_id"].(string)
		}
		last, _ := msg["last_agent_message"].(string)
		return []Event{{Type: "result", Text: last, SessionID: sid}}
	}
	return nil
}
