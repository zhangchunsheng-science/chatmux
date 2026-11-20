// Config validation for `chatmux check`: report everything wrong with
// a config file without starting anything. Findings are human-readable
// lines; each names the section it came from.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/cliagent"
	"github.com/zhangchunsheng-science/chatmux/internal/cron"
)

// knownProviders mirrors the registry wired in main at boot.
var knownProviders = map[string]bool{"openai": true, "anthropic": true}

// Validate returns problems (must fix before boot) and warnings (worth
// knowing: docwatch targets not yet on disk, ...). Empty problems means
// the config is sound.
func Validate(cfg *Config) (problems, warnings []string) {
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	channels := map[string]bool{}
	for i, c := range cfg.Channels {
		name := c.Name
		if name == "" {
			name = c.Type
		}
		if c.Type == "" {
			add("channels[%d]: type is required", i)
			continue
		}
		if channels[name] {
			add("channels[%d] %q: duplicate name", i, name)
		}
		channels[name] = true
	}
	// The web console channel is always attached at boot (v0.4.6+), so
	// routes/cron/docwatch may target "web" even with channels[] empty.
	channels["web"] = true
	if len(cfg.Channels) == 0 {
		warn("channels: none configured — running as a web-console gateway (dashboard chat, admin API, MCP)")
	}

	// dataDir probe: when the dir can't be created or written, the usage
	// logger silently disables itself (usage.New ignores open errors) and
	// accounting is lost without any boot failure — surface it up front.
	// A warning, not a problem: check may run as a different user than
	// the service, so this is advisory.
	dir := cfg.DataDir
	if dir == "" {
		dir = "data"
	}
	if err := probeWritable(dir); err != nil {
		warn("dataDir %q: %v — usage logging would be silently disabled", dir, err)
	}

	agents := map[string]bool{}
	for i, a := range cfg.Agents {
		if a.Name == "" {
			add("agents[%d]: name is required", i)
			continue
		}
		if agents[a.Name] {
			add("agents[%d] %q: duplicate name", i, a.Name)
		}
		agents[a.Name] = true
		if a.Provider != "" && !knownProviders[a.Provider] {
			add("agents[%d] %q: unknown provider %q (openai|anthropic)", i, a.Name, a.Provider)
		}
		if a.TurnTimeout != "" {
			if d, err := time.ParseDuration(a.TurnTimeout); err != nil || d <= 0 {
				add("agents[%d] %q: bad turnTimeout %q (e.g. \"45m\")", i, a.Name, a.TurnTimeout)
			}
		}
		// model-key probe: an openai/anthropic agent with neither its
		// own apiKey nor the provider env var will 401 on every turn at
		// runtime. Advisory like the dataDir probe: check may run in a
		// different shell than the service, so env here is a hint, not
		// the truth.
		if a.Cli == nil && a.APIKey == "" {
			p := a.Provider
			if p == "" {
				p = "openai" // runtime default
			}
			env := map[string]string{"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY"}[p]
			if env != "" && os.Getenv(env) == "" {
				warn("agents[%d] %q: no apiKey and %s not set — model calls would fail auth at runtime", i, a.Name, env)
			}
		}
		if a.Cli != nil {
			if err := cliagent.CheckConfig(*a.Cli); err != nil {
				add("agents[%d] %q: %v", i, a.Name, err)
			}
		}
		for j, rt := range a.Routes {
			if rt.Channel != "" && rt.Channel != "any" && !channels[rt.Channel] {
				add("agents[%d] %q routes[%d]: unknown channel %q", i, a.Name, j, rt.Channel)
			}
		}
	}
	if len(cfg.Agents) == 0 {
		add("agents: none configured")
	}
	if cfg.DefaultAgent != "" && !agents[cfg.DefaultAgent] {
		add("defaultAgent %q: no such agent", cfg.DefaultAgent)
	}

	for i, c := range cfg.Cron {
		if c.Name == "" {
			add("cron[%d]: name is required", i)
		}
		if _, err := cron.Parse(c.Schedule); err != nil {
			add("cron[%d] %q: %v", i, c.Name, err)
		}
		if !channels[c.Channel] {
			add("cron[%d] %q: unknown channel %q", i, c.Name, c.Channel)
		}
		if c.ChatID == "" {
			add("cron[%d] %q: chatId is required", i, c.Name)
		}
		if c.Text == "" {
			add("cron[%d] %q: text is required", i, c.Name)
		}
	}

	for i, w := range cfg.Workflows {
		if w.Name == "" {
			add("workflows[%d]: name is required", i)
		}
		if len(w.Steps) == 0 {
			add("workflows[%d] %q: no steps", i, w.Name)
		}
		if w.Agent != "" && !agents[w.Agent] {
			add("workflows[%d] %q: unknown agent %q", i, w.Name, w.Agent)
		}
		for j, s := range w.Steps {
			if s.Agent != "" && !agents[s.Agent] {
				add("workflows[%d] %q steps[%d]: unknown agent %q", i, w.Name, j, s.Agent)
			}
			if s.Prompt == "" {
				add("workflows[%d] %q steps[%d]: prompt is required", i, w.Name, j)
			}
		}
	}

	for i, d := range cfg.DocWatch {
		if d.Name == "" {
			add("docwatch[%d]: name is required", i)
		}
		if d.Path == "" {
			add("docwatch[%d] %q: path is required", i, d.Name)
		} else if _, err := os.Stat(d.Path); err != nil {
			warn("docwatch[%d] %q: path %q not on disk yet — docwatch will wait for it to appear", i, d.Name, d.Path)
		}
		if !channels[d.Channel] {
			add("docwatch[%d] %q: unknown channel %q", i, d.Name, d.Channel)
		}
		if d.ChatID == "" {
			add("docwatch[%d] %q: chatId is required", i, d.Name)
		}
	}

	if cfg.I18n != nil && cfg.I18n.Locale != "" && cfg.I18n.Locale != "zh" && cfg.I18n.Locale != "en" {
		add("i18n: unsupported locale %q (zh|en)", cfg.I18n.Locale)
	}
	if cfg.HTTP.Listen == "" {
		add("http: listen is required (e.g. \":8080\")")
	}
	return problems, warnings
}

// probeWritable reports whether dir can be written (created if missing,
// via its parent — boot MkdirAlls it) by touching and removing a probe
// file.
func probeWritable(dir string) error {
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(dir)
		if _, err := os.Stat(parent); err != nil {
			return fmt.Errorf("parent %s not on disk: %v", parent, err)
		}
		dir = parent
	}
	probe := filepath.Join(dir, ".chatmux-check-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		return fmt.Errorf("not writable: %v", err)
	}
	return os.Remove(probe)
}
