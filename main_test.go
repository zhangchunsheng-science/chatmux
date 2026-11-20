package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestLoadSkills(t *testing.T) {
	dir := t.TempDir()
	// flat skill
	os.WriteFile(filepath.Join(dir, "flat.md"), []byte("直接正文"), 0o644)
	// flat skill with frontmatter
	os.WriteFile(filepath.Join(dir, "meta.md"), []byte("---\nname: x\ndescription: '一段描述'\n---\n正文内容"), 0o644)
	// directory skill (Claude style)
	os.Mkdir(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "SKILL.md"), []byte("---\ndescription: 目录技能\n---\n包技能正文"), 0o644)
	// non-skill noise
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(dir, "empty"), 0o755)

	bodies, descs := loadSkills(dir)
	if len(bodies) != 3 {
		t.Fatalf("want 3 skills, got %d: %v", len(bodies), bodies)
	}
	if bodies["flat"] != "直接正文" {
		t.Errorf("flat: %q", bodies["flat"])
	}
	if bodies["meta"] != "正文内容" {
		t.Errorf("meta body should be frontmatter-stripped: %q", bodies["meta"])
	}
	if descs["meta"] != "一段描述" || descs["pkg"] != "目录技能" {
		t.Errorf("descs: %+v", descs)
	}
	if _, ok := bodies["empty"]; ok {
		t.Error("dir without SKILL.md should not load")
	}
	if _, ok := bodies["notes"]; ok {
		t.Error("non-md should not load")
	}
}

func TestStripFrontmatterNoBlock(t *testing.T) {
	body, desc := stripFrontmatter("just text")
	if body != "just text" || desc != "" {
		t.Fatalf("%q %q", body, desc)
	}
	body, desc = stripFrontmatter("---\nno closing fence")
	if body != "---\nno closing fence" || desc != "" {
		t.Fatalf("unclosed frontmatter should pass through: %q", body)
	}
}

func TestNextBackoff(t *testing.T) {
	step := minChannelBackoff
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, maxChannelBackoff, maxChannelBackoff}
	for _, w := range want {
		step = nextBackoff(step)
		if step != w {
			t.Fatalf("backoff = %s, want %s", step, w)
		}
	}
	// already at cap stays at cap
	if got := nextBackoff(maxChannelBackoff); got != maxChannelBackoff {
		t.Fatalf("cap not held: %s", got)
	}
}

// TestReadmeDocumentsAllCommands guards docs drift: every command the
// runtime dispatches on (case "/x" in agent.go) must appear in the
// README 聊天内命令 table. New command → forgotten README row is now
// a test failure, not a discovery for the reader.
func TestReadmeDocumentsAllCommands(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("internal", "agent", "agent.go"))
	if err != nil {
		t.Fatal(err)
	}
	cmdRe := regexp.MustCompile(`case "(/[a-z]+)"`)
	cmds := map[string]bool{}
	for _, m := range cmdRe.FindAllStringSubmatch(string(src), -1) {
		cmds[m[1]] = true
	}
	if len(cmds) < 20 { // sanity: the scan must find the dispatch table
		t.Fatalf("scan found only %d commands — dispatch moved?", len(cmds))
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	section := readme
	if i := bytes.Index(section, []byte("## 聊天内命令")); i >= 0 {
		section = section[i:]
		if j := bytes.Index(section[3:], []byte("\n## ")); j >= 0 {
			section = section[:j+3]
		}
	} else {
		t.Fatal("README 聊天内命令 section not found")
	}
	for cmd := range cmds {
		if !bytes.Contains(section, []byte("`"+cmd)) {
			t.Errorf("README 聊天内命令 table is missing %s", cmd)
		}
	}
}
