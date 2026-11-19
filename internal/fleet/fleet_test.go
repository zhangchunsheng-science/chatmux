package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndGet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.json")
	if err := os.WriteFile(path, []byte(`{"instances":[
		{"name":"a","baseUrl":"http://a","token":"ta"},
		{"name":"b","baseUrl":"http://b"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if in, err := c.Get("b"); err != nil || in.BaseURL != "http://b" || in.Token != "" {
		t.Fatalf("get b: %+v err=%v", in, err)
	}
	if _, err := c.Get("nope"); err == nil {
		t.Fatal("unknown instance should error")
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file should error")
	}
}

func TestProbeAndList(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/status" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"version":"0.4.9","agents":3,"channels":{"feishu":true,"tg":false}}`))
	}))
	defer up.Close()

	c := &Config{Instances: []Instance{
		{Name: "z-down", BaseURL: "http://127.0.0.1:1"},
		{Name: "a-up", BaseURL: up.URL},
	}}
	list := c.List(context.Background())
	if len(list) != 2 {
		t.Fatalf("len %d", len(list))
	}
	if list[0].Name != "a-up" || !list[0].Reachable || list[0].Version != "0.4.9" ||
		list[0].ChannelsUp != 1 || list[0].ChannelsAll != 2 || list[0].Agents != 3 {
		t.Fatalf("up summary: %+v", list[0])
	}
	if list[1].Reachable || list[1].Err == "" {
		t.Fatalf("down summary: %+v", list[1])
	}
}

func TestCallProxy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Admin-Token") != "tok" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/admin/send" && r.Method == http.MethodPost {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	c := &Config{Instances: []Instance{{Name: "one", BaseURL: ts.URL, Token: "tok"}}}
	body, code, err := c.Call(context.Background(), "one", http.MethodPost, "/admin/send",
		map[string]string{"channel": "feishu", "chatId": "oc_1", "text": "hi"})
	if err != nil || code != 200 {
		t.Fatalf("code=%d err=%v body=%s", code, err, body)
	}
}
