package discord

import (
	"encoding/json"
	"testing"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/dedup"
)

func mustMsg(t *testing.T, payload string) channel.Message {
	t.Helper()
	d := &Discord{name: "discord", seen: dedup.New(t.TempDir()+"/seen.json", 0, 10)}
	m, ok := d.toMessage(json.RawMessage(payload))
	if !ok {
		t.Fatal("message not accepted")
	}
	return m
}

func TestToMessageAttachmentTypes(t *testing.T) {
	m := mustMsg(t, `{
		"id": "1", "content": "语音",
		"author": {"id": "u1", "username": "u"},
		"attachments": [
			{"url": "https://cdn.example/a.png", "filename": "a.png", "content_type": "image/png"},
			{"url": "https://cdn.example/v.ogg", "filename": "v.ogg", "content_type": "audio/ogg"},
			{"url": "https://cdn.example/doc.bin", "filename": "doc.bin", "content_type": "application/octet-stream"},
			{"url": "https://cdn.example/legacy.PNG", "filename": "legacy.PNG"}
		]
	}`)
	want := []string{"image", "audio", "file", "image"}
	if len(m.Attachments) != len(want) {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	for i, w := range want {
		if m.Attachments[i].Type != w {
			t.Fatalf("attachment %d type = %s, want %s", i, m.Attachments[i].Type, w)
		}
	}
}
