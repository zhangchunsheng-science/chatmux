// Config validation for `chatmux check`: report everything wrong with
// a config file without starting anything. Findings are human-readable
// lines; each names the section it came from.
package config

import (
	"fmt"
	"os"
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
