package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/config"
	"github.com/zhangchunsheng-science/chatmux/internal/metrics"
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

	// /metrics carries per-agent traffic and error counts — with a
	// token set it must sit behind the same guard as /admin/* (header
	// or ?token=, so Prometheus scrape configs just add the param).
	resp, err := client.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /metrics without token = %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	req.Header.Set("X-Admin-Token", "t")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics with token = %d, want 200", resp.StatusCode)
	}
}

// panicChan panics on its first Start (malformed upstream payload),
// then behaves like a healthy channel until cancelled.
type panicChan struct{ calls int32 }

func (p *panicChan) Name() string { return "panic" }
func (p *panicChan) Start(ctx context.Context, _ chan<- channel.Message) error {
	if atomic.AddInt32(&p.calls, 1) == 1 {
		panic("bad payload")
	}
	<-ctx.Done()
	return nil
}
func (p *panicChan) SenderFor(channel.Message) (channel.Sender, error) { return nil, errors.New("n/a") }
func (p *panicChan) Healthy() bool                                     { return true }

// superviseChannel converts a channel-adapter panic into an error and
// reconnects: one malformed payload must cost a reconnect, not the
// whole process (all channels + web console).
func TestSuperviseChannelSurvivesPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc := &panicChan{}
	done := make(chan struct{})
	go func() {
		superviseChannel(ctx, pc, make(chan channel.Message), 5*time.Millisecond, 20*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&pc.calls) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n := atomic.LoadInt32(&pc.calls); n < 2 {
		t.Fatalf("channel restarted %d times after panic, want ≥2 — panic would have killed the process before", n)
	}
	if _, _, cr := metrics.Snapshot(); cr < 1 {
		t.Fatalf("channel reconnect counter = %d, want ≥1 after the panic restart", cr)
	}
}

// TestMetricsRendersAllSeries guards the /metrics rendering: every
// chatmux_* metric family declared in the handler source must appear
// in live output. A rendering regression (renamed family, dropped
// series, broken condition) silently deletes a series dashboards
// depend on — this makes it a test failure instead. Exceptions:
// feedback_up/down need a real 👍/👎 press to exist at all.
func TestMetricsRendersAllSeries(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	families := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`# TYPE (chatmux_[a-z_]+)`),
		regexp.MustCompile(`gauge\("(chatmux_[a-z_]+)"`),
	} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			families[m[1]] = true
		}
	}
	if len(families) < 10 {
		t.Fatalf("scan found only %d metric families — handler moved?", len(families))
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cm.json")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := `{"defaultAgent":"a","dataDir":` + strconv.Quote(filepath.ToSlash(dir)) + `,
	  "http":{"listen":"127.0.0.1:` + strconv.Itoa(port) + `","adminToken":"t"},
	  "agents":[{"name":"a","cli":{"type":"generic","bin":"/bin/echo"}}]}`
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
		t.Skip("server did not come up — metrics assertions skipped")
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	req.Header.Set("X-Admin-Token", "t") // harmless if /metrics is open
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET /metrics = %d", resp.StatusCode)
	}
	for f := range families {
		if f == "chatmux_feedback_up_total" || f == "chatmux_feedback_down_total" {
			continue // need a real feedback press to render
		}
		if !bytes.Contains(body, []byte(f)) {
			t.Errorf("/metrics is missing series %s", f)
		}
	}
}

// stubChan is a config-free channel for handler-level tests.
type stubChan struct {
	name    string
	healthy bool
}

func (s *stubChan) Name() string { return s.name }
func (s *stubChan) Start(ctx context.Context, _ chan<- channel.Message) error {
	<-ctx.Done()
	return nil
}
func (s *stubChan) SenderFor(channel.Message) (channel.Sender, error) {
	return nil, errors.New("n/a")
}
func (s *stubChan) Healthy() bool { return s.healthy }

func TestHealthzBody(t *testing.T) {
	if code, body := healthzBody(map[string]channel.Channel{}); code != 200 || body != "ok" {
		t.Fatalf("no channels: %d %q", code, body)
	}
	all := map[string]channel.Channel{
		"feishu": &stubChan{name: "feishu", healthy: true},
		"tg1":    &stubChan{name: "tg1", healthy: false},
		"slack":  &stubChan{name: "slack", healthy: false},
	}
	code, body := healthzBody(all)
	if code != 503 || body != "degraded: slack, tg1" {
		t.Fatalf("degraded body = %d %q, want 503 %q (sorted, named)", code, body, "degraded: slack, tg1")
	}
}

// The init starter must be immediately bootable: valid JSON, zero
// check problems, a real token, and safe to refuse overwriting.
func TestWriteInitConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chatmux.json")
	token, err := writeInitConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 24 {
		t.Fatalf("token too short: %q", token)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("init config does not load: %v", err)
	}
	if cfg.HTTP.AdminToken != token || cfg.DefaultAgent != "echo" || len(cfg.Agents) != 1 {
		t.Fatalf("init config fields wrong: %+v", cfg.HTTP)
	}
	if _, err := writeInitConfig(path); err == nil {
		t.Fatal("second init must refuse to overwrite")
	}
	if p, _ := config.Validate(cfg); len(p) != 0 {
		t.Fatalf("init config has check problems: %v", p)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("init config must be owner-only, got %v", info.Mode())
	}
}

// A bind failure must be a clean exit code, not log.Fatalf: the test
// binary (and any embedder) survives. This was the root cause of two
// mystery CI failures — the ephemeral port a test just released got
// re-grabbed before runServer could bind it, and Fatalf killed the
// whole test process with no --- FAIL annotation.
func TestRunServerBindFailureReturns(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind: %v", err)
	}
	defer ln.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cm.json")
	cfg := `{"defaultAgent":"a","dataDir":` + strconv.Quote(filepath.ToSlash(dir)) + `,
	  "http":{"listen":"` + ln.Addr().String() + `","adminToken":"t"},
	  "agents":[{"name":"a","cli":{"type":"generic","bin":"/bin/echo"}}]}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	code := make(chan int, 1)
	go func() { code <- runServer(cfgPath) }()
	select {
	case c := <-code:
		if c != 1 {
			t.Fatalf("runServer on taken port = %d, want 1", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServer did not return on bind failure — would it Fatalf?")
	}
}

// TestRunFleetTimeoutsGuardHangs pins the fleet client-side deadlines: a
// wedged gateway (accepts, never answers) must fail status fast and fail
// ask after -timeout+grace — not hang the CLI until Ctrl-C.
func TestRunFleetTimeoutsGuardHangs(t *testing.T) {
	// wedge: answers after 2s — far past the 150ms client deadline, but
	// never past the test itself (a channel-release pattern deadlocks in
	// hang.Close(), which waits for outstanding handlers).
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer hang.Close()
	dir := t.TempDir()
	fleetPath := filepath.Join(dir, "fleet.json")
	if err := os.WriteFile(fleetPath, []byte(`{"instances":[{"name":"w","baseUrl":"`+hang.URL+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHATMUX_FLEET", fleetPath)

	oldQuick, oldGrace := clientQuickTimeout, clientAskGrace
	clientQuickTimeout, clientAskGrace = 150*time.Millisecond, 150*time.Millisecond
	defer func() { clientQuickTimeout, clientAskGrace = oldQuick, oldGrace }()

	done := make(chan int, 2)
	start := time.Now()
	go func() { done <- runFleet([]string{"status", "w"}) }()
	go func() { done <- runFleet([]string{"ask", "w", "-to", "web:c1", "-timeout", "0", "在吗"}) }()
	for i := 0; i < 2; i++ {
		select {
		case c := <-done:
			if c != 1 {
				t.Fatalf("exit = %d, want 1", c)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("wedged gateway hung the fleet CLI — client deadline not wired")
		}
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("took %s; deadlines not honored", elapsed)
	}
}

// TestRunFleetAskOptForms pins repeatable -opt parsing: both "-opt A" and
// "-opt=B" must reach the gateway as options (the old manual scan silently
// dropped the equals form).
func TestRunFleetAskOptForms(t *testing.T) {
	var mu sync.Mutex
	var got []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Options []string `json:"options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = req.Options
		mu.Unlock()
		w.Write([]byte(`{"answer":"ok"}`))
	}))
	defer ts.Close()
	dir := t.TempDir()
	fleetPath := filepath.Join(dir, "fleet.json")
	if err := os.WriteFile(fleetPath, []byte(`{"instances":[{"name":"ok","baseUrl":"`+ts.URL+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHATMUX_FLEET", fleetPath)

	if code := runFleet([]string{"ask", "ok", "-to", "web:c1", "-opt=A", "-opt", "B", "选哪个"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("options = %v, want [A B]", got)
	}
}

// TestAdminReqHonorsContextDeadline pins the direct client's ctx plumbing:
// a wedged gateway must surface a ctx error fast, not hang the CLI.
func TestAdminReqHonorsContextDeadline(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer hang.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := adminReq(ctx, http.MethodGet, hang.URL+"/admin/status", "t", nil); err == nil {
		t.Fatal("wedged gateway should error")
	} else if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("adminReq took %s; deadline not honored", elapsed)
	}
}

// TestRunClientAskHangsAndOptForms exercises the real ask client: a wedged
// gateway fails within the (shrunken) deadline instead of hanging, and both
// -opt spellings reach the gateway as options.
func TestRunClientAskHangsAndOptForms(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer hang.Close()
	hangHost := strings.TrimPrefix(hang.URL, "http://")

	var mu sync.Mutex
	var got []string
	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Options []string `json:"options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = req.Options
		mu.Unlock()
		w.Write([]byte(`{"answer":"ok"}`))
	}))
	defer rec.Close()
	recHost := strings.TrimPrefix(rec.URL, "http://")

	cfgFor := func(listen string) string {
		p := filepath.Join(t.TempDir(), "cm.json")
		cfg := `{"defaultAgent":"a","http":{"listen":"` + listen + `","adminToken":"t"},
		  "agents":[{"name":"a","cli":{"type":"generic","bin":"/bin/echo"}}]}`
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	oldGrace := clientAskGrace
	clientAskGrace = 150 * time.Millisecond
	defer func() { clientAskGrace = oldGrace }()

	// wedged gateway: -timeout 0 → deadline = 0 + grace
	start := time.Now()
	if code := runClient([]string{"ask", "-to", "web:c1", "-timeout", "0", "在吗"}, cfgFor(hangHost)); code != 1 {
		t.Fatalf("wedged ask exit = %d, want 1", code)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("wedged ask took %s; client deadline not wired", elapsed)
	}

	// live gateway: both -opt spellings land as options
	if code := runClient([]string{"ask", "-to", "web:c1", "-opt=A", "-opt", "B", "选哪个"}, cfgFor(recHost)); code != 0 {
		t.Fatalf("live ask exit = %d, want 0", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("options = %v, want [A B]", got)
	}
}
