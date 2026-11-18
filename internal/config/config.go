// Package config loads chatmux.json — the single config file describing
// IM channels, agents (model wiring) and routing.
package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/zhangchunsheng-science/chatmux/internal/cliagent"
)

// Config is the whole chatmux.json document.
type Config struct {
	Channels []ChannelConfig `json:"channels"`
	Agents   []AgentConfig   `json:"agents"`
	// DefaultAgent is used for chats with no explicit route.
	DefaultAgent string `json:"defaultAgent"`
	// HTTP server for webhook channels (WeCom) and /healthz.
	HTTP HTTPConfig `json:"http"`
	// DataDir stores session history and state.
	DataDir string `json:"dataDir"`
	// Cron schedules fire synthetic messages at agents.
	Cron []CronConfig `json:"cron"`
}

// ChannelConfig is one IM adapter instance. Type selects the adapter;
// every other field is passed to it as raw options so adapters can evolve
// without breaking the core schema.
type ChannelConfig struct {
	Type    string          `json:"type"` // feishu|dingtalk|wecom|telegram|discord|slack
	Name    string          `json:"name"` // instance nickname, defaults to Type
	Options json.RawMessage `json:"options"`
}

// AgentConfig is one bot persona: which model it thinks with, its system
// prompt, and which chats it serves.
type AgentConfig struct {
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled,omitempty"`
	Model   string `json:"model,omitempty"`
	System  string `json:"system,omitempty"`
	// Provider: openai|anthropic|... (a provider name registered at boot)
	Provider string `json:"provider,omitempty"`
	// APIKey/BaseURL overrides for this agent.
	APIKey  string `json:"apiKey,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
	// Routes: chats this agent serves; first match wins.
	Routes []Route `json:"routes,omitempty"`
	// Cli switches this agent to CLI mode: instead of calling a model
	// API directly it drives a headless coding CLI (claude/codex/generic)
	// per conversation, resuming the CLI's own session between turns.
	Cli *cliagent.Config `json:"cli,omitempty"`
	// WorkingDir for CLI-agent mode.
	WorkingDir string `json:"workingDir,omitempty"`
	// GroupMention: when false, answer every group message without
	// requiring an @-mention. Default (nil/true): groups must @ the bot.
	GroupMention *bool `json:"groupMention,omitempty"`
}

// Route binds chats to this agent.
type Route struct {
	Channel  string `json:"channel,omitempty"` // channel name or "any"
	ChatID   string `json:"chatId,omitempty"`  // exact chat id, or "p2p"/"group"
	ChatType string `json:"chatType,omitempty"`
	UserID   string `json:"userId,omitempty"` // only this user wakes this agent
}

// HTTPConfig is the inbound HTTP server (webhook channels + ops).
type HTTPConfig struct {
	Listen string `json:"listen"` // ":8080"
	// AdminToken guards /admin/* endpoints. Empty: only loopback-safe
	// default — set a token for anything exposed beyond localhost.
	AdminToken string `json:"adminToken,omitempty"`
}

// CronConfig is one scheduled job: fire Text at an agent's chat on a
// 5-field cron schedule (min hour dom mon dow).
type CronConfig struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"` // "30 9 * * *"
	Channel  string `json:"channel"`  // channel name from channels[]
	ChatID   string `json:"chatId"`   // target chat id
	ThreadID string `json:"threadId,omitempty"`
	Text     string `json:"text"` // prompt fired at the agent
	// Enabled defaults true.
	Enabled *bool `json:"enabled,omitempty"`
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = "127.0.0.1:8080"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	for i := range c.Channels {
		if c.Channels[i].Name == "" {
			c.Channels[i].Name = c.Channels[i].Type
		}
	}
}

func (c *Config) validate() error {
	if len(c.Channels) == 0 {
		return fmt.Errorf("config: no channels configured")
	}
	seen := map[string]bool{}
	for _, ch := range c.Channels {
		if seen[ch.Name] {
			return fmt.Errorf("config: duplicate channel name %q", ch.Name)
		}
		seen[ch.Name] = true
	}
	if len(c.Agents) == 0 && c.DefaultAgent == "" {
		return fmt.Errorf("config: no agents configured")
	}
	for _, a := range c.Agents {
		if a.Name == "" {
			return fmt.Errorf("config: agent with empty name")
		}
	}
	return nil
}
