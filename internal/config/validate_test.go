package config

import (
	"strings"
	"testing"
)

func TestValidateCleanConfig(t *testing.T) {
	cfg := &Config{
		Channels: []ChannelConfig{{Type: "feishu", Name: "fs", Options: []byte(`{"appId":"a","appSecret":"s"}`)}},
		Agents: []AgentConfig{
			{Name: "helper", Provider: "openai", TurnTimeout: "45m"},
			{Name: "coder", Provider: "anthropic", Routes: []Route{{Channel: "fs"}}},
		},
		DefaultAgent: "helper",
		HTTP:         HTTPConfig{Listen: ":8080"},
		Cron:         []CronConfig{{Name: "daily", Schedule: "30 9 * * *", Channel: "fs", ChatID: "c1", Text: "早报"}},
	}
	if got, warn := Validate(cfg); len(got) != 0 || len(warn) != 0 {
		t.Fatalf("clean config flagged: %v %v", got, warn)
	}
}

func TestValidateBrokenConfig(t *testing.T) {
	cfg := &Config{
		Channels: []ChannelConfig{{Type: "feishu", Name: "fs"}, {Type: "feishu", Name: "fs"}},
		Agents: []AgentConfig{
			{Name: "a", Provider: "nope", TurnTimeout: "soonish"},
			{Name: "a", Routes: []Route{{Channel: "ghost"}}},
		},
		DefaultAgent: "missing",
		Cron:         []CronConfig{{Name: "bad", Schedule: "not cron", Channel: "ghost", ChatID: "", Text: ""}},
		Workflows:    []WorkflowConfig{{Name: "w", Steps: []WorkflowStep{{Agent: "ghost", Prompt: ""}}}},
		I18n:         &I18nConfig{Locale: "fr"},
	}
	got := strings.Join(mustProblems(t, cfg), "\n")
	for _, want := range []string{
		"duplicate name",     // channel dup
		"unknown provider",   // agent provider
		"bad turnTimeout",    // duration
		"unknown channel",    // route + cron + workflow paths
		"no such agent",      // defaultAgent
		"want 5 fields",      // cron spec
		"chatId is required", // cron
		"unsupported locale", // i18n
		"listen is required", // http
		"duplicate name",     // agent dup
		"prompt is required", // workflow step
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing finding %q in:\n%s", want, got)
		}
	}
}

func TestValidateDocwatchPathIsWarning(t *testing.T) {
	cfg := &Config{
		Channels: []ChannelConfig{{Type: "feishu", Name: "fs", Options: []byte(`{}`)}},
		Agents:   []AgentConfig{{Name: "a"}},
		HTTP:     HTTPConfig{Listen: ":8080"},
		DocWatch: []DocWatchConfig{{Name: "later", Path: "/nonexistent/plan.md", Channel: "fs", ChatID: "c1", Prompt: "p"}},
	}
	problems, warnings := Validate(cfg)
	if len(problems) != 0 {
		t.Fatalf("missing docwatch path must be a warning, got problems %v", problems)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "wait for it") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestValidateNoChannelsIsWarning(t *testing.T) {
	// The web console channel is always attached at boot, so a config
	// with zero IM channels is a valid gateway-only setup: warning, not
	// a boot-blocking problem.
	cfg := &Config{
		Agents:       []AgentConfig{{Name: "a", Provider: "openai"}},
		DefaultAgent: "a",
		HTTP:         HTTPConfig{Listen: ":8080"},
	}
	problems, warnings := Validate(cfg)
	if len(problems) != 0 {
		t.Fatalf("zero channels must not be a problem, got %v", problems)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "web-console") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func mustProblems(t *testing.T, cfg *Config) []string {
	t.Helper()
	p, _ := Validate(cfg)
	return p
}
