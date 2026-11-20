package docwatch

import (
	"bytes"
	"fmt"
	"log"
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

// An edit near the top of a long file must be visible in the excerpt.
// The old tail-only 4000-line cap diffed only the file's last 4000
// lines, so edits above them were silently reported as no change.
func TestDiffEarlyChangeVisible(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 1; i <= 5000; i++ {
		if i == 2 {
			oldB.WriteString("line-two-original\n")
			newB.WriteString("line-two-CHANGED\n")
		} else {
			fmt.Fprintf(&oldB, "line-%d\n", i)
			fmt.Fprintf(&newB, "line-%d\n", i)
		}
	}
	d := DiffExcerpt(oldB.String(), newB.String(), 10)
	if !strings.Contains(d, "- line-two-original") || !strings.Contains(d, "+ line-two-CHANGED") {
		t.Errorf("early edit lost from diff:\n%.300s", d)
	}
	if strings.Contains(d, "line-4000") {
		t.Errorf("unchanged far-away lines leaked into excerpt:\n%.300s", d)
	}
}

// A wholesale rewrite of a huge file must stay cheap and honest: the
// middle is capped to 1000 lines/side before an int32 DP (the old
// 4001×4001 int table allocated ~128MB per such poll), and the header
// must flag that the counts cover an excerpt, not the whole rewrite.
func TestDiffHugeRewriteBounded(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&oldB, "old-%d\n", i)
		fmt.Fprintf(&newB, "new-%d\n", i)
	}
	d := DiffExcerpt(oldB.String(), newB.String(), 60)
	if !strings.Contains(d, "节选") {
		t.Errorf("huge rewrite should be flagged as excerpted:\n%.200s", d)
	}
	if !strings.Contains(d, "- old-") { // walk emits deletes first when LCS is empty
		t.Errorf("excerpt missing change lines:\n%.200s", d)
	}
	// pure insertion: the '+' path of the same capped machinery
	d = DiffExcerpt("base\n", newB.String(), 60)
	if !strings.Contains(d, "节选") || !strings.Contains(d, "+ new-") {
		t.Errorf("huge insertion excerpt wrong:\n%.200s", d)
	}
}

// A watched file losing readability mid-run must surface in the log
// exactly once (not once per tick), and recovery must be logged too —
// otherwise a permission change silently freezes the watcher forever.
func TestPollUnreadableTransitionLogged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	w := New(nil, func(string, string) {})
	e := config.DocWatchConfig{Name: "notes", Path: path}
	w.Poll(e) // baseline
	if s := buf.String(); s != "" {
		t.Fatalf("baseline poll logged: %q", s)
	}

	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	w.Poll(e)
	first := buf.String()
	if !strings.Contains(first, "cannot read") {
		t.Fatalf("unreadable transition not logged: %q", first)
	}
	buf.Reset()
	w.Poll(e) // still unreadable: silent
	if s := buf.String(); s != "" {
		t.Fatalf("repeat unreadable poll logged again: %q", s)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.Poll(e)
	if s := buf.String(); !strings.Contains(s, "readable again") {
		t.Fatalf("recovery not logged: %q", s)
	}
}
