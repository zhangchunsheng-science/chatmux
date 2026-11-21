package config

import (
	"net"
	"os"
	"strings"
	"testing"

	"github.com/zhangchunsheng-science/chatmux/internal/cliagent"
)

func TestValidateCleanConfig(t *testing.T) {
	cfg := &Config{
		Channels: []ChannelConfig{{Type: "feishu", Name: "fs", Options: []byte(`{"appId":"a","appSecret":"s"}`)}},
		Agents: []AgentConfig{
			{Name: "helper", Provider: "openai", APIKey: "k", TurnTimeout: "45m"},
			{Name: "coder", Provider: "anthropic", APIKey: "k", Routes: []Route{{Channel: "fs"}}},
		},
		DefaultAgent: "helper",
		HTTP:         HTTPConfig{Listen: testListen(t)},
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
		Agents:   []AgentConfig{{Name: "a", APIKey: "k"}},
		HTTP:     HTTPConfig{Listen: testListen(t)},
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
		Agents:       []AgentConfig{{Name: "a", Provider: "openai", APIKey: "k"}},
		DefaultAgent: "a",
		HTTP:         HTTPConfig{Listen: testListen(t)},
	}
	problems, warnings := Validate(cfg)
	if len(problems) != 0 {
		t.Fatalf("zero channels must not be a problem, got %v", problems)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "web-console") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestValidateWebChannelRoutesOK(t *testing.T) {
	// "web" is always attached at boot — routes/cron targeting it must
	// not be reported as unknown channels.
	cfg := &Config{
		Agents:       []AgentConfig{{Name: "a", Routes: []Route{{Channel: "web"}}}},
		DefaultAgent: "a",
		HTTP:         HTTPConfig{Listen: testListen(t)},
		Cron:         []CronConfig{{Name: "c", Schedule: "0 9 * * *", Channel: "web", ChatID: "console", Text: "早"}},
		DataDir:      t.TempDir(),
	}
	if problems, _ := Validate(cfg); len(problems) != 0 {
		t.Fatalf("web-channel routes flagged: %v", problems)
	}
}

func TestValidateDataDirNotWritable(t *testing.T) {
	// an unwritable dataDir silently disables usage logging at boot —
	// check must warn about it up front
	dir := t.TempDir()
	sub := dir + "/data"
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Agents:       []AgentConfig{{Name: "a"}},
		DefaultAgent: "a",
		HTTP:         HTTPConfig{Listen: testListen(t)},
		DataDir:      sub,
	}
	problems, warnings := Validate(cfg)
	if len(problems) != 0 {
		t.Fatalf("unwritable dataDir is advisory, not a problem: %v", problems)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "usage logging") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no dataDir warning among %v", warnings)
	}
}

func TestValidateAgentKeyProbe(t *testing.T) {
	// a model agent with neither its own apiKey nor the provider env
	// var will 401 on every turn at runtime — check warns up front.
	// t.Setenv("") is "unset" for os.Getenv purposes and auto-restores.
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	hasKeyWarn := func(ws []string) bool {
		for _, w := range ws {
			if strings.Contains(w, "fail auth") {
				return true
			}
		}
		return false
	}

	cfg := &Config{
		Agents:       []AgentConfig{{Name: "a"}}, // provider defaults to openai
		DefaultAgent: "a",
		HTTP:         HTTPConfig{Listen: testListen(t)},
		DataDir:      t.TempDir(),
	}
	if _, warnings := Validate(cfg); !hasKeyWarn(warnings) {
		t.Fatalf("keyless openai agent not warned: %v", warnings)
	}

	cfg.Agents[0].Provider = "anthropic"
	_, warnings := Validate(cfg)
	if !hasKeyWarn(warnings) {
		t.Fatalf("keyless anthropic agent not warned: %v", warnings)
	}
	namedEnv := false
	for _, w := range warnings {
		if strings.Contains(w, "ANTHROPIC_API_KEY") {
			namedEnv = true
		}
	}
	if !namedEnv {
		t.Fatalf("anthropic agent should name ANTHROPIC_API_KEY: %v", warnings)
	}

	cfg.Agents[0].APIKey = "k" // own key suppresses the warning
	if _, warnings := Validate(cfg); hasKeyWarn(warnings) {
		t.Fatalf("agent apiKey set but still warned: %v", warnings)
	}

	t.Setenv("ANTHROPIC_API_KEY", "env-key") // env presence also suppresses
	cfg.Agents[0].APIKey = ""
	if _, warnings := Validate(cfg); hasKeyWarn(warnings) {
		t.Fatalf("env key set but still warned: %v", warnings)
	}

	// CLI agents drive a local binary — no model key needed.
	cfg.Agents[0] = AgentConfig{Name: "a", Cli: &cliagent.Config{Type: "generic", Bin: "/bin/echo"}}
	if _, warnings := Validate(cfg); hasKeyWarn(warnings) {
		t.Fatalf("CLI agent warned about model key: %v", warnings)
	}
}

// testListen returns a free loopback address so the listen probe never
// collides with whatever else is running on the machine.
func testListen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot grab a port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func mustProblems(t *testing.T, cfg *Config) []string {
	t.Helper()
	p, _ := Validate(cfg)
	return p
}

func TestValidateListenInUseWarns(t *testing.T) {
	// hold a real port so the probe must fail: boot would die with a
	// bare "address already in use", check names it up front
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind: %v", err)
	}
	defer ln.Close()
	cfg := &Config{
		Agents:       []AgentConfig{{Name: "a", APIKey: "k"}},
		DefaultAgent: "a",
		HTTP:         HTTPConfig{Listen: ln.Addr().String()},
		DataDir:      t.TempDir(),
	}
	problems, warnings := Validate(cfg)
	if len(problems) != 0 {
		t.Fatalf("busy port is advisory: %v", problems)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "fail to bind") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no bind warning among %v", warnings)
	}
}

func TestValidateUnknownProviderSuggests(t *testing.T) {
	cfg := &Config{
		Agents:       []AgentConfig{{Name: "a", Provider: "opena", APIKey: "k"}},
		DefaultAgent: "a",
		HTTP:         HTTPConfig{Listen: testListen(t)},
		DataDir:      t.TempDir(),
	}
	got := strings.Join(mustProblems(t, cfg), "\n")
	if !strings.Contains(got, "openai") || !strings.Contains(got, "did you mean") {
		t.Fatalf("provider typo should suggest: %v", got)
	}
}

func TestLoopbackListen(t *testing.T) {
	for listen, want := range map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		"":               true, // Load defaults to 127.0.0.1:8080
		":8080":          false,
		"0.0.0.0:8080":   false,
		"192.168.1.5:80": false,
	} {
		if got := loopbackListen(listen); got != want {
			t.Errorf("loopbackListen(%q) = %v, want %v", listen, got, want)
		}
	}
}

// An empty adminToken on a non-loopback listener is an open gateway:
// /admin/chat would spend the API keys, /admin/stop and /admin/send
// are one HTTP call away for anyone on the network. check must name
// that before boot — and stay silent when either condition lifts.
func TestValidateOpenAdminWarns(t *testing.T) {
	_, port, err := net.SplitHostPort(testListen(t))
	if err != nil {
		t.Skipf("no port: %v", err)
	}
	base := func(listen string) *Config {
		return &Config{
			HTTP:   HTTPConfig{Listen: listen},
			Agents: []AgentConfig{{Name: "a", Provider: "openai", APIKey: "k"}},
		}
	}
	for _, c := range []struct {
		listen string
		token  string
		want   bool
	}{
		{"0.0.0.0:" + port, "", true},
		{":" + port, "", true},
		{"0.0.0.0:" + port, "t", false},
		{"127.0.0.1:" + port, "", false},
	} {
		cfg := base(c.listen)
		cfg.HTTP.AdminToken = c.token
		_, warnings := Validate(cfg)
		got := false
		for _, w := range warnings {
			if strings.Contains(w, "adminToken") {
				got = true
			}
		}
		if got != c.want {
			t.Errorf("listen=%q token=%q: adminToken warning = %v, want %v (warnings: %v)", c.listen, c.token, got, c.want, warnings)
		}
	}
}

func TestValidateAttachmentRetention(t *testing.T) {
	base := func(ret string) *Config {
		return &Config{
			AttachmentRetention: ret,
			Agents:              []AgentConfig{{Name: "a", Provider: "openai", APIKey: "k"}},
			HTTP:                HTTPConfig{Listen: testListen(t)},
		}
	}
	if p, _ := Validate(base("junk")); len(p) == 0 {
		t.Error("bad attachmentRetention must be a problem")
	}
	if p, _ := Validate(base("-1h")); len(p) == 0 {
		t.Error("negative attachmentRetention must be a problem")
	}
	for _, ok := range []string{"72h", "0", "0s", ""} {
		if p, _ := Validate(base(ok)); len(p) != 0 {
			t.Errorf("attachmentRetention %q flagged: %v", ok, p)
		}
	}
}

func TestValidateTurnRatePerMinute(t *testing.T) {
	base := func(n int) *Config {
		return &Config{
			TurnRatePerMinute: n,
			Agents:            []AgentConfig{{Name: "a", Provider: "openai", APIKey: "k"}},
			HTTP:              HTTPConfig{Listen: testListen(t)},
		}
	}
	if p, _ := Validate(base(-1)); len(p) == 0 {
		t.Error("negative turnRatePerMinute must be a problem")
	}
	for _, ok := range []int{0, 1, 60} {
		if p, _ := Validate(base(ok)); len(p) != 0 {
			t.Errorf("turnRatePerMinute %d flagged: %v", ok, p)
		}
	}
}
