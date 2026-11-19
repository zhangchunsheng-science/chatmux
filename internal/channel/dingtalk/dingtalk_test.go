package dingtalk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/event"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
)

func newTestDingTalk(t *testing.T, opts Options) *DingTalk {
	t.Helper()
	if opts.ClientID == "" {
		opts.ClientID = "k"
		opts.ClientSecret = "s"
	}
	opts.DataDir = t.TempDir()
	opts.APIBase = "https://example.invalid" // overridden by sender tests
	ch, err := New(mustJSON(t, opts))
	if err != nil {
		t.Fatal(err)
	}
	return ch.(*DingTalk)
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandleEventClassification(t *testing.T) {
	d := newTestDingTalk(t, Options{RobotCode: "rc1", BotUserID: "$:LWCP^bot"})
	hdr := func(id, typ string) *event.EventHeader {
		return &event.EventHeader{EventId: id, EventType: typ}
	}

	// robot itself added → bot_added
	m, ok := d.handleEvent(hdr("e1", "chat_add_member"),
		[]byte(`{"conversationId":"cid1","addUserIds":["$:LWCP^bot","u9"],"operatorStaffId":"op"}`))
	if !ok || m.Event != "bot_added" {
		t.Fatalf("want bot_added, got ok=%v ev=%q", ok, m.Event)
	}
	if m.ChatID != "cid1" || m.ChatType != "group" || m.UserID != "op" {
		t.Errorf("message fields: %+v", m)
	}
	if tg, okT := m.Raw.(*ProactiveTarget); !okT || tg.ConversationID != "cid1" || tg.RobotCode != "rc1" {
		t.Errorf("raw target: %+v", m.Raw)
	}

	// human-only add → member_added
	m, _ = d.handleEvent(hdr("e2", "chat_add_member"),
		[]byte(`{"conversationId":"cid1","addUserIds":["u2"]}`))
	if m.Event != "member_added" {
		t.Fatalf("want member_added, got %q", m.Event)
	}

	// robot removed (matches robotCode) → bot_removed
	m, _ = d.handleEvent(hdr("e3", "chat_remove_member"),
		[]byte(`{"conversationId":"cid1","delUserIds":["rc1"]}`))
	if m.Event != "bot_removed" {
		t.Fatalf("want bot_removed, got %q", m.Event)
	}

	// human-only removal → member_removed
	m, _ = d.handleEvent(hdr("e4", "chat_remove_member"),
		[]byte(`{"conversationId":"cid1","delUserIds":["u3"]}`))
	if m.Event != "member_removed" {
		t.Fatalf("want member_removed, got %q", m.Event)
	}

	// disband → group_disbanded even without member lists
	m, _ = d.handleEvent(hdr("e5", "chat_disband"), []byte(`{"conversationId":"cid1"}`))
	if m.Event != "group_disbanded" {
		t.Fatalf("want group_disbanded, got %q", m.Event)
	}

	// unrelated event type ignored
	if _, ok = d.handleEvent(hdr("e6", "org_user_add_robot"), nil); ok {
		t.Fatal("non-group event should not emit")
	}

	// duplicate delivery dropped
	if _, ok = d.handleEvent(hdr("e1", "chat_add_member"),
		[]byte(`{"conversationId":"cid1","addUserIds":["$:LWCP^bot"]}`)); ok {
		t.Fatal("duplicate eventId should be deduped")
	}
}

func TestHandleEventNoIdentityConfigured(t *testing.T) {
	d := newTestDingTalk(t, Options{}) // no robotCode, no botUserId
	m, _ := d.handleEvent(&event.EventHeader{EventId: "x1", EventType: "chat_add_member"},
		[]byte(`{"conversationId":"cid","addUserIds":["whatever"]}`))
	if m.Event != "member_added" {
		t.Fatalf("unidentifiable add must stay member_added, got %q", m.Event)
	}
}

func TestProactiveSender(t *testing.T) {
	var gotTokenReq, gotSendReq bool
	var sendBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			gotTokenReq = true
			json.NewEncoder(w).Encode(map[string]any{"accessToken": "tok123", "expireIn": 7200})
		case "/v1.0/robot/groupMessages/send":
			gotSendReq = true
			if r.Header.Get("x-acs-dingtalk-access-token") != "tok123" {
				t.Error("missing access token header")
			}
			json.NewDecoder(r.Body).Decode(&sendBody)
			json.NewEncoder(w).Encode(map[string]string{"messageId": "m1"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := newTestDingTalk(t, Options{RobotCode: "rc1"})
	d.opts.APIBase = srv.URL
	s := &proactiveSender{d: d, t: &ProactiveTarget{ConversationID: "cid9", RobotCode: "rc1"}}
	id, err := s.Send(context.Background(), "欢迎！", channel.SendOptions{Final: true})
	if err != nil {
		t.Fatal(err)
	}
	if !gotTokenReq || !gotSendReq {
		t.Fatalf("token=%v send=%v", gotTokenReq, gotSendReq)
	}
	if id != "m1" {
		t.Errorf("message id: %q", id)
	}
	if sendBody["msgKey"] != "sampleMarkdown" || sendBody["openConversationId"] != "cid9" || sendBody["robotCode"] != "rc1" {
		t.Errorf("send body: %+v", sendBody)
	}
	var p map[string]string
	json.Unmarshal([]byte(sendBody["msgParam"]), &p)
	if p["text"] != "欢迎！" {
		t.Errorf("msgParam: %+v", p)
	}

	// missing robotCode → clear error, no request
	s2 := &proactiveSender{d: d, t: &ProactiveTarget{ConversationID: "cid9"}}
	if _, err := s2.Send(context.Background(), "x", channel.SendOptions{Final: true}); err == nil {
		t.Fatal("want error without robotCode")
	}
}
