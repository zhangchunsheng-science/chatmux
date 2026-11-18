package cliagent

import (
	"encoding/json"
	"testing"
)

func TestParseClaudeEvents(t *testing.T) {
	// assistant record with text + tool_use blocks
	line := `{"type":"assistant","message":{"content":[
		{"type":"text","text":"looking at the repo"},
		{"type":"tool_use","name":"Read","input":{}}
	]}}`
	evs := parseJSONLine("claude", []byte(line))
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != "text" || evs[0].Text != "looking at the repo" {
		t.Errorf("ev0 = %+v", evs[0])
	}
	if evs[1].Type != "tool" || evs[1].Text != "🔧 Read" {
		t.Errorf("ev1 = %+v", evs[1])
	}

	// result record carries session_id for resume
	line = `{"type":"result","result":"done, 3 files changed","session_id":"abc-123"}`
	evs = parseJSONLine("claude", []byte(line))
	if len(evs) != 1 || evs[0].Type != "result" || evs[0].SessionID != "abc-123" || evs[0].Text != "done, 3 files changed" {
		t.Fatalf("result event wrong: %+v", evs)
	}
}

func TestParseCodexEvents(t *testing.T) {
	line := `{"msg":{"type":"agent_message","message":"halfway done"}}`
	evs := parseJSONLine("codex", []byte(line))
	if len(evs) != 1 || evs[0].Type != "text" || evs[0].Text != "halfway done" {
		t.Fatalf("agent_message wrong: %+v", evs)
	}

	line = `{"msg":{"type":"token_count","input_tokens":10},"session_id":"s1"}`
	if evs := parseJSONLine("codex", []byte(line)); len(evs) != 0 {
		t.Fatalf("token_count should be dropped: %+v", evs)
	}

	line = `{"msg":{"type":"task_complete","last_agent_message":"finished"},"session_id":"s2"}`
	evs = parseJSONLine("codex", []byte(line))
	if len(evs) != 1 || evs[0].Type != "result" || evs[0].SessionID != "s2" || evs[0].Text != "finished" {
		t.Fatalf("task_complete wrong: %+v", evs)
	}
}

func TestParseGenericText(t *testing.T) {
	line, _ := json.Marshal(map[string]any{"text": "hello"})
	evs := parseJSONLine("generic", line)
	if len(evs) != 1 || evs[0].Text != "hello" {
		t.Fatalf("generic wrong: %+v", evs)
	}
}

func TestResumeIDLifecycle(t *testing.T) {
	r := NewRunner()
	if got := r.ResumeID("a|b"); got != "" {
		t.Fatalf("fresh runner should have no resume id, got %q", got)
	}
	r.setResumeID("a|b", "sid-1")
	if got := r.ResumeID("a|b"); got != "sid-1" {
		t.Fatalf("resume id = %q", got)
	}
	r.Reset("a|b")
	if got := r.ResumeID("a|b"); got != "" {
		t.Fatalf("after Reset resume id = %q", got)
	}
	// empty id never stored
	r.setResumeID("a|b", "")
	if got := r.ResumeID("a|b"); got != "" {
		t.Fatalf("empty id should not overwrite, got %q", got)
	}
}
