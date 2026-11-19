package docwatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhangchunsheng-science/chatmux/internal/config"
)

func TestDiffExcerpt(t *testing.T) {
	oldD := "a\nb\nc"
	newD := "a\nx\nc\nd"
	d := DiffExcerpt(oldD, newD, 10)
	for _, want := range []string{"- b", "+ x", "+ d"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "- a") || strings.Contains(d, "- c") {
		t.Errorf("diff should keep unchanged lines out:\n%s", d)
	}
	if DiffExcerpt("same", "same", 10) != "（无行级差异）\n" &&
		!strings.Contains(DiffExcerpt("same", "same", 10), "无行级差异") {
		t.Error("identical content should report no line diff")
	}
}

func TestDiffExcerptCap(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 100; i++ {
		a.WriteString("old\n")
		b.WriteString("new\n")
	}
	d := DiffExcerpt(a.String(), b.String(), 10)
	if !strings.Contains(d, "其余省略") {
		t.Errorf("cap marker missing:\n%.200s", d)
	}
	if got := strings.Count(d, "\n+ ") + strings.Count(d, "\n- "); got > 12 {
		t.Errorf("too many shown lines: %d", got)
	}
}

func TestPollFiresOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(path, []byte("v1\n计划 A"), 0o644); err != nil {
		t.Fatal(err)
	}
	var fired []string
	w := New([]config.DocWatchConfig{{
		Name: "plan", Path: path, Channel: "web", ChatID: "console",
		Prompt: "总结变更：{{diff}}",
	}}, func(name, prompt string) {
		fired = append(fired, name+"|"+prompt)
	})

	w.Poll(config.DocWatchConfig{Name: "plan", Path: path}) // baseline only
	if len(fired) != 0 {
		t.Fatalf("first sighting must not fire: %v", fired)
	}
	if err := os.WriteFile(path, []byte("v2\n计划 B\n新增任务"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.Poll(config.DocWatchConfig{Name: "plan", Path: path, Prompt: "总结变更：{{diff}}"})
	if len(fired) != 1 {
		t.Fatalf("change should fire once: %v", fired)
	}
	if !strings.Contains(fired[0], "+ 新增任务") || !strings.Contains(fired[0], "- 计划 A") {
		t.Errorf("prompt diff wrong: %s", fired[0])
	}
	// no change → silent
	w.Poll(config.DocWatchConfig{Name: "plan", Path: path, Prompt: "x {{diff}}"})
	if len(fired) != 1 {
		t.Fatalf("no-change poll fired: %v", fired)
	}
	// default prompt
	if err := os.WriteFile(path, []byte("v3"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.Poll(config.DocWatchConfig{Name: "plan", Path: path})
	if len(fired) != 2 || !strings.Contains(fired[1], "plan") {
		t.Fatalf("default prompt wrong: %v", fired)
	}
}
