// Package cliagent drives external coding-CLI agents (claude, codex, or
// any line-oriented binary) headlessly — chatmux's counterpart to
// botmux's worker/PTY machinery, implemented via each CLI's native
// non-interactive streaming mode instead of screen scraping.
package cliagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Event is one incremental output from a running CLI agent.
type Event struct {
	Type string `json:"type"` // "text" | "tool" | "result" | "error"
	Text string `json:"text"`
	// SessionID is set on "result": the CLI's own session id to resume.
	SessionID string `json:"sessionId"`
	Err       error  `json:"-"`
}

// Runner spawns one CLI process per turn (headless CLIs own their own
// conversational memory via resume ids; chatmux tracks the ids).
type Runner struct {
	mu       sync.Mutex
	sessions map[string]string // chatmux anchor key → CLI session id
}

func NewRunner() *Runner {
	return &Runner{sessions: map[string]string{}}
}

// ResumeID returns the stored CLI session id for an anchor.
func (r *Runner) ResumeID(anchor string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[anchor]
}

func (r *Runner) setResumeID(anchor, id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[anchor] = id
}

// Reset forgets the CLI session for an anchor (next turn starts fresh).
func (r *Runner) Reset(anchor string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, anchor)
}

// Config describes how to invoke one CLI.
type Config struct {
	Type       string `json:"type"`  // claude | codex | generic
	Bin        string `json:"bin"`   // executable; defaults per type
	Model      string `json:"model"` // passed through when supported
	WorkingDir string `json:"workingDir"`
	// generic type:
	PromptArg  bool   `json:"promptArg"`  // pass prompt as argv (else stdin)
	ResumeFlag string `json:"resumeFlag"` // e.g. "--resume"
}

// Run executes one turn: builds the command line, streams events.
// anchor keys the resume memory. Cancelling ctx kills the process.
func (r *Runner) Run(ctx context.Context, cfg Config, anchor, prompt string) (<-chan Event, error) {
	bin := cfg.Bin
	var args []string
	promptViaStdin := true

	switch cfg.Type {
	case "claude", "":
		if cfg.Type == "" {
			cfg.Type = "claude"
		}
		if bin == "" {
			bin = "claude"
		}
		args = []string{"-p", "--output-format", "stream-json", "--verbose"}
		if resume := r.ResumeID(anchor); resume != "" {
			args = append(args, "--resume", resume)
		}
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
	case "codex":
		if bin == "" {
			bin = "codex"
		}
		args = []string{"exec", "--json"}
		if resume := r.ResumeID(anchor); resume != "" {
			args = append(args, "resume", resume)
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
	case "generic":
		if bin == "" {
			return nil, fmt.Errorf("cliagent: generic runner needs bin")
		}
		promptViaStdin = !cfg.PromptArg
		if cfg.PromptArg {
			args = append(args, prompt)
		}
		if resume := r.ResumeID(anchor); resume != "" && cfg.ResumeFlag != "" {
			args = append(args, cfg.ResumeFlag, resume)
		}
	default:
		return nil, fmt.Errorf("cliagent: unknown type %q", cfg.Type)
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
