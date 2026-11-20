package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

func TestDownloadSendsBotToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("pngbytes"))
	}))
	defer srv.Close()

	s := &Slack{opts: Options{BotToken: "xoxb-test"}, client: srv.Client()}
	data, mime, err := s.Download(context.Background(), channel.Attachment{Type: "image", URL: srv.URL + "/file.png"})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if gotAuth != "Bearer xoxb-test" {
		t.Fatalf("bot token not sent, auth = %q", gotAuth)
	}
	if string(data) != "pngbytes" || mime != "image/png" {
		t.Fatalf("got %q %q", data, mime)
	}
}

func TestDownloadErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	s := &Slack{opts: Options{BotToken: "xoxb-test"}, client: srv.Client()}
	if _, _, err := s.Download(context.Background(), channel.Attachment{URL: srv.URL + "/f"}); err == nil {
		t.Fatal("want error on 403")
	}
	if _, _, err := s.Download(context.Background(), channel.Attachment{}); err == nil {
		t.Fatal("want error on empty url")
	}
}
