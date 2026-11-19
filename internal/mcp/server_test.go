package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveQuestions spins a fake admin API and runs one Serve round.
func roundTrip(t *testing.T, srv *Server, lines []string) []string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/status":
			w.Write([]byte(`{"channels":{"feishu":true}}`))
		case "/admin/send":
			w.Write([]byte(`{}`))
		case "/admin/ask":
			w.Write([]byte(`{"answer":"方案B"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	srv.base = ts.URL
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out strings.Builder
	if err := srv.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return strings.Split(strings.TrimSpace(out.String()), "\n")
}

func TestInitializeAndList(t *testing.T) {
	srv := New("http://x", "")
	out := roundTrip(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	})
	if len(out) != 2 {
		t.Fatalf("want 2 responses (notification skipped), got %d: %v", len(out), out)
	}
	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out[0]), &init); err != nil {
		t.Fatal(err)
	}
	if init.Result.ProtocolVersion != protocolVersion || init.Result.ServerInfo.Name != "chatmux" {
		t.Fatalf("bad initialize result: %s", out[0])
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out[1]), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Result.Tools) != 4 {
		t.Fatalf("want 4 tools, got %d", len(list.Result.Tools))
	}
	got := map[string]bool{}
	for _, tl := range list.Result.Tools {
		got[tl.Name] = true
	}
	for _, want := range []string{"chatmux_send", "chatmux_ask", "chatmux_status", "chatmux_history"} {
		if !got[want] {
			t.Fatalf("missing tool %s", want)
		}
	}
}

func TestCallTools(t *testing.T) {
	srv := New("http://x", "tok")
	out := roundTrip(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chatmux_send","arguments":{"channel":"feishu","chatId":"oc_1","text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"chatmux_ask","arguments":{"channel":"feishu","chatId":"oc_1","question":"选哪个？","options":["A","B"]}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"chatmux_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	})
	if len(out) != 4 {
		t.Fatalf("want 4 responses, got %d: %v", len(out), out)
	}
	textOf := func(line string) (string, bool) {
		var r struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		return r.Result.Content[0].Text, r.Result.IsError
	}
	if s, e := textOf(out[0]); s != "已发送 ✓" || e {
		t.Fatalf("send: %q err=%v", s, e)
	}
	if s, e := textOf(out[1]); s != "方案B" || e {
		t.Fatalf("ask: %q err=%v", s, e)
	}
	if s, e := textOf(out[2]); !strings.Contains(s, `"feishu":true`) || e {
		t.Fatalf("status: %q err=%v", s, e)
	}
	if s, e := textOf(out[3]); !strings.Contains(s, "unknown tool") || !e {
		t.Fatalf("unknown tool should be error: %q err=%v", s, e)
	}
}

func TestMethodNotFoundAndPing(t *testing.T) {
	srv := New("http://x", "")
	out := roundTrip(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"bogus/method"}`,
		`not json`,
	})
	if len(out) != 3 {
		t.Fatalf("want 3 responses, got %d: %v", len(out), out)
	}
	if !strings.Contains(out[0], `"result":{}`) {
		t.Fatalf("ping: %s", out[0])
	}
	if !strings.Contains(out[1], "-32601") {
		t.Fatalf("not found: %s", out[1])
	}
	if !strings.Contains(out[2], "-32700") {
		t.Fatalf("parse error: %s", out[2])
	}
}
