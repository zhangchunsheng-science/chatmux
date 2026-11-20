package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	l.Log(Record{Agent: "a", Mode: "api", InputTokens: 10, OutputTokens: 20, OutputChars: 100})
	l.Log(Record{Agent: "a", Mode: "cli:claude", OutputChars: 50})
	l.Log(Record{Agent: "b", Mode: "api", InputTokens: 1, OutputTokens: 2})
	l.Log(Record{Agent: "feedback", Mode: "feedback:up", Channel: "feishu"})
	l.Log(Record{Agent: "feedback", Mode: "feedback:down", Channel: "feishu"})
	l.Close()

	aggs, err := Summarize(dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Agg{}
	for _, a := range aggs {
		byName[a.Agent] = a
	}
	a := byName["a"]
	if a.Calls != 2 || a.InputTokens != 10 || a.OutputTokens != 20 || a.OutputChars != 150 {
		t.Errorf("agent a agg wrong: %+v", a)
	}
	fb := byName["feedback"]
	if fb.FeedbackUp != 1 || fb.FeedbackDown != 1 || fb.Calls != 0 {
		t.Errorf("feedback agg wrong: %+v", fb)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage.jsonl")); err != nil {
		t.Errorf("usage.jsonl missing: %v", err)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	out, err := Summarize(t.TempDir())
	if err != nil || out != nil {
		t.Errorf("empty dir should give nil,nil; got %v %v", out, err)
	}
}

func TestSummarizeSince(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	l.Log(Record{Agent: "old", Mode: "api", InputTokens: 1})
	l.Close()
	// backdate the first row, keep a fresh one
	p := filepath.Join(dir, "usage.jsonl")
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var oldRec Record
	json.Unmarshal([]byte(lines[0]), &oldRec)
	oldRec.Time = time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
	ob, _ := json.Marshal(oldRec)
	fresh, _ := json.Marshal(Record{Time: time.Now().Format(time.RFC3339), Agent: "new", Mode: "api", InputTokens: 5})
	os.WriteFile(p, append(append(ob, '\n'), append(fresh, '\n')...), 0o644)

	aggs, err := SummarizeSince(dir, time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatal(err)
	}
	if len(aggs) != 1 || aggs[0].Agent != "new" || aggs[0].InputTokens != 5 {
		t.Fatalf("window filter wrong: %+v", aggs)
	}
	all, _ := Summarize(dir)
	if len(all) != 2 {
		t.Fatalf("zero since must keep all rows: %+v", all)
	}
}

func TestSummarizeErrors(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	l.Log(Record{Agent: "a", Mode: "api", InputTokens: 5})
	l.Log(Record{Agent: "a", Mode: "api", Error: "401 unauthorized"})
	l.Close()
	aggs, err := Summarize(dir)
	if err != nil || len(aggs) != 1 {
		t.Fatalf("aggs: %v %v", aggs, err)
	}
	if aggs[0].Calls != 2 || aggs[0].Errors != 1 {
		t.Fatalf("want 2 calls 1 error, got %+v", aggs[0])
	}
}

func TestSummarizeAvgMs(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	l.Log(Record{Agent: "a", Mode: "api", DurationMs: 300})
	l.Log(Record{Agent: "a", Mode: "api", DurationMs: 700})
	l.Close()
	aggs, err := Summarize(dir)
	if err != nil || len(aggs) != 1 {
		t.Fatalf("aggs: %v %v", aggs, err)
	}
	if aggs[0].AvgMs != 500 {
		t.Fatalf("avg = %d, want 500", aggs[0].AvgMs)
	}
}

func TestLoggerRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	l := &Logger{f: f, maxBytes: 100}
	for _, name := range []string{"rot-r1", "rot-r2", "rot-r3"} {
		l.Log(Record{Agent: name, Mode: "api"})
	}
	l.Close()

	cur, _ := os.ReadFile(path)
	if strings.Contains(string(cur), "rot-r1") {
		t.Fatalf("oldest generation should have rotated away: %s", cur)
	}
	if !strings.Contains(string(cur), "rot-r3") {
		t.Fatalf("newest record missing from current log: %s", cur)
	}
	bak, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("rotated backup missing: %v", err)
	}
	if !strings.Contains(string(bak), "rot-r2") || strings.Contains(string(bak), "rot-r1") {
		t.Fatalf("backup should hold exactly the middle generation: %s", bak)
	}
	aggs, err := Summarize(dir)
	if err != nil || len(aggs) != 1 || aggs[0].Agent != "rot-r3" {
		t.Fatalf("Summarize after rotation = %v, %v", aggs, err)
	}
}

func TestLoggerNoRotationWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	l := New(dir) // production cap: 10MB — three records must not rotate
	for i := 0; i < 3; i++ {
		l.Log(Record{Agent: "keeper", Mode: "api"})
	}
	l.Close()
	b, err := os.ReadFile(filepath.Join(dir, "usage.jsonl"))
	if err != nil || !strings.Contains(string(b), "keeper") {
		t.Fatalf("records lost: %v %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage.jsonl.1")); !os.IsNotExist(err) {
		t.Fatalf("unexpected rotation at tiny size: %v", err)
	}
}
