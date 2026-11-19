package wecom

import (
	"encoding/xml"
	"testing"
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
