package cliagent

import (
	"strings"
	"testing"
)

func TestBuildInvocationClaudeResume(t *testing.T) {
	bin, args, stdin, err := buildInvocation(Config{Type: "claude", Model: "opus"}, "sess-123", "")
	if err != nil {
		t.Fatal(err)
	}
	if bin != "claude" || !stdin {
		t.Fatalf("bin=%q stdin=%v", bin, stdin)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p", "--output-format stream-json", "--verbose", "--resume sess-123", "--model opus"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}

func TestBuildInvocationGenericNeedsBin(t *testing.T) {
	if _, _, _, err := buildInvocation(Config{Type: "generic"}, "", ""); err == nil {
		t.Fatal("generic without bin should error")
	}
}

func TestSandboxDockerWrap(t *testing.T) {
	cfg := Config{
		Type:       "claude",
		WorkingDir: "/tmp/proj",
		Sandbox:    &SandboxConfig{Mode: "docker", Image: "node:20-slim"},
	}
	bin, args, _, err := buildInvocation(cfg, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if bin != "docker" {
		t.Fatalf("bin=%q want docker", bin)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"run --rm -i --network none", "-v /tmp/proj:/work -w /work", "node:20-slim claude"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	// tail must be the CLI's own argv
	if !strings.HasSuffix(joined, "claude -p --output-format stream-json --verbose") {
		t.Errorf("CLI argv not preserved at tail: %q", joined)
	}
}

func TestSandboxNetworkOverride(t *testing.T) {
	cfg := Config{Type: "codex", Sandbox: &SandboxConfig{Mode: "docker", Network: "host"}}
	_, args, _, err := buildInvocation(cfg, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "--network host") {
		t.Errorf("network override ignored: %v", args)
	}
}
