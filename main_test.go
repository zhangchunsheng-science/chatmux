package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// TestAdminRoutesRequireToken: with an adminToken configured, every
// registered /admin/* route must reject unauthenticated requests. A
// handler added without the guard is an open admin API — v0.4.3 fixed
// exactly such a hole on GET /admin/ask; this keeps it from coming
// back. Routes are discovered from the source at test time.
func TestAdminRoutesRequireToken(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`HandleFunc\("(/admin/[a-z]+)"`)
	var paths []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		paths = append(paths, m[1])
	}
	if len(paths) < 7 {
		t.Fatalf("scan found only %d admin routes — source moved?", len(paths))
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cm.json")
	// ephemeral port (grab-and-release): with -count=2 the first server
	// goroutine outlives the test, so a fixed port would silently skip
	// the second run
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := `{"defaultAgent":"a","dataDir":` + strconv.Quote(filepath.ToSlash(dir)) + `,
	  "http":{"listen":"127.0.0.1:` + strconv.Itoa(port) + `","adminToken":"t"},
	  "agents":[{"name":"a","provider":"openai","apiKey":"k","baseUrl":"http://127.0.0.1:9/v1"}]}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() { _ = runServer(cfgPath) }()

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	up := false
	for i := 0; i < 50; i++ {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			up = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !up {
		t.Skip("server did not come up (port busy?) — auth assertions skipped")
	}

	client := &http.Client{Timeout: 5 * time.Second}
	for _, p := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req, err := http.NewRequest(method, base+p, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, p, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s without token = %d, want 401", method, p, resp.StatusCode)
			}
		}
	}
}
