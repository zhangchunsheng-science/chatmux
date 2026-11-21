package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicRequestBaseURLBeatsDefault(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if !strings.Contains(r.URL.Path, "/messages") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"gw\"}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	a := &Anthropic{APIKey: "sk-test"} // no struct BaseURL
	ch, err := a.Stream(context.Background(), Request{
		Model:    "claude-test",
		BaseURL:  srv.URL,
		Messages: []Msg{{Role: User, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for d := range ch {
		if d.Err != nil {
			t.Fatal(d.Err)
		}
	}
	if !reached {
		t.Fatal("request never reached the gateway URL — Request.BaseURL ignored")
	}
}
