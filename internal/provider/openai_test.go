package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIBuildsStreamRequest(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("auth header %q", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	o := &OpenAI{APIKey: "sk-test", BaseURL: srv.URL, Client: srv.Client()}
	ch, err := o.Stream(context.Background(), Request{
		Model:    "test-model",
		System:   "be nice",
		Messages: []Msg{{Role: User, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for d := range ch {
		if d.Err != nil {
			t.Fatal(d.Err)
		}
		sb.WriteString(d.Text)
	}
	if sb.String() != "你好" {
		t.Fatalf("streamed text = %q", sb.String())
	}
	if gotBody["model"] != "test-model" {
		t.Fatalf("model = %v", gotBody["model"])
	}
	msgs := gotBody["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatal("system prompt must be first message")
	}
}

// Regression: the agent-level baseUrl (Request.BaseURL — how configs
// point agents at DeepSeek/vLLM/one-api gateways) must beat the
// provider struct default. It used to be silently ignored: every
// gateway-configured agent actually hit api.openai.com.
func TestOpenAIRequestBaseURLBeatsDefault(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"gw\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	o := &OpenAI{APIKey: "sk-test"} // no struct BaseURL: default would be api.openai.com
	ch, err := o.Stream(context.Background(), Request{
		Model:    "m",
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
