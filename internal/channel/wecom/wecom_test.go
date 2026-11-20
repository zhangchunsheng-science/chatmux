package wecom

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

func toPlain(t *testing.T, raw string) cbPlain {
	t.Helper()
	var p cbPlain
	if err := xml.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestToEventMessage(t *testing.T) {
	w := &WeCom{name: "wecom"}

	cases := []struct {
		xml  string
		want string
	}{
		{`<xml><MsgType>event</MsgType><Event>change_external_chat</Event><ChangeType>create</ChangeType><ChatId>wrk1</ChatId><CreateTime>100</CreateTime></xml>`, "group_created"},
		{`<xml><MsgType>event</MsgType><Event>change_external_chat</Event><ChangeType>dismiss</ChangeType><ChatId>wrk1</ChatId><CreateTime>101</CreateTime></xml>`, "group_disbanded"},
		{`<xml><MsgType>event</MsgType><Event>change_external_chat</Event><ChangeType>update</ChangeType><UpdateDetail>add_member</UpdateDetail><ChatId>wrk1</ChatId><CreateTime>102</CreateTime></xml>`, "member_added"},
		{`<xml><MsgType>event</MsgType><Event>chat.update</Event><ChangeType>update</ChangeType><UpdateDetail>del_member</UpdateDetail><ChatId>wrk1</ChatId><CreateTime>103</CreateTime></xml>`, "member_removed"},
		{`<xml><MsgType>event</MsgType><Event>change_external_chat</Event><ChangeType>update</ChangeType><UpdateDetail>change_name</UpdateDetail><ChatId>wrk1</ChatId><CreateTime>104</CreateTime></xml>`, "group_updated"},
	}
	for _, c := range cases {
		p := toPlain(t, c.xml)
		m, ok := w.toEventMessage(p)
		if !ok {
			t.Fatalf("%s: not ok", c.want)
		}
		if m.Event != c.want {
			t.Errorf("want %s, got %s", c.want, m.Event)
		}
		if m.ChatID != "wrk1" || m.ChatType != "group" {
			t.Errorf("fields: %+v", m)
		}
	}

	// unrelated events ignored
	if _, ok := w.toEventMessage(toPlain(t, `<xml><MsgType>event</MsgType><Event>subscribe</Event><ChatId>wrk1</ChatId></xml>`)); ok {
		t.Error("unrelated event should not emit")
	}
	// no chat id → unusable
	if _, ok := w.toEventMessage(toPlain(t, `<xml><MsgType>event</MsgType><Event>change_external_chat</Event><ChangeType>create</ChangeType></xml>`)); ok {
		t.Error("missing ChatId should not emit")
	}
}

func TestChatMessageStillParses(t *testing.T) {
	// group @ message must keep flowing through toMessage unchanged
	p := toPlain(t, `<xml><MsgType>text</MsgType><Content> 你好 </Content><ChatId>wrk1</ChatId><FromUserName>u1</FromUserName><MsgId>1</MsgId><CreateTime>9</CreateTime></xml>`)
	w := &WeCom{name: "wecom"}
	m := w.toMessage(p)
	if m.Text != "你好" || m.ChatType != "group" || m.ChatID != "wrk1" {
		t.Fatalf("chat message parse: %+v", m)
	}
}

func wecomTestServer(t *testing.T, media map[string][]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/cgi-bin/gettoken"):
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"tok","expires_in":7200}`))
		case strings.HasSuffix(r.URL.Path, "/cgi-bin/media/get"):
			id := r.URL.Query().Get("media_id")
			data, ok := media[id]
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errcode":40058,"errmsg":"invalid media_id"}`))
				return
			}
			w.Header().Set("Content-Type", "audio/amr")
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestDownload(t *testing.T) {
	srv := wecomTestServer(t, map[string][]byte{"VOICE1": []byte("amrbytes")})
	defer srv.Close()
	w := &WeCom{opts: Options{CorpID: "corp", Secret: "sec", APIBase: srv.URL}, client: srv.Client()}

	data, mime, err := w.Download(context.Background(), channel.Attachment{Type: "audio", Key: "VOICE1"})
	if err != nil || string(data) != "amrbytes" || mime != "audio/amr" {
		t.Fatalf("download = %q %q %v", data, mime, err)
	}
	if _, _, err := w.Download(context.Background(), channel.Attachment{Type: "audio", Key: "NOPE"}); err == nil {
		t.Fatal("want error for unknown media_id")
	}
	if _, _, err := w.Download(context.Background(), channel.Attachment{Type: "audio"}); err == nil {
		t.Fatal("want error for empty media id")
	}
}
