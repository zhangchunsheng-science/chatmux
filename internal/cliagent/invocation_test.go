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

func TestBuildInvocationArgvPresets(t *testing.T) {
	cases := []struct {
		typ, model, wantBin, wantArgs string
	}{
		{"gemini", "gemini-2.0-flash", "gemini", "-p hi -m gemini-2.0-flash"},
		{"qwen", "qwen3-coder-plus", "qwen", "-p hi -m qwen3-coder-plus"},
		{"opencode", "claude-sonnet-5-5", "opencode", "run hi --model claude-sonnet-5-5"},
		{"aider", "gpt-4o", "aider", "--yes-always --no-auto-commits --message hi --model gpt-4o"},
	}
	for _, c := range cases {
		bin, args, stdin, err := buildInvocation(Config{Type: c.typ, Model: c.model}, "", "hi")
		if err != nil {
			t.Fatal(err)
		}
		if bin != c.wantBin || stdin {
			t.Fatalf("%s: bin=%q stdin=%v", c.typ, bin, stdin)
		}
		got := strings.Join(args, " ")
		if !strings.Contains(got, c.wantArgs) {
			t.Errorf("%s: args %q missing %q", c.typ, got, c.wantArgs)
		}
	}
}

func TestBuildInvocationUnknownType(t *testing.T) {
	if _, _, _, err := buildInvocation(Config{Type: "bogus"}, "", ""); err == nil {
		t.Fatal("unknown type should error")
	}
}
