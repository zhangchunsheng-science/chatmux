package feishu

import "testing"

func TestFormatMeeting(t *testing.T) {
	cases := []struct {
		n    meetingNote
		want string
	}{
		{meetingNote{kind: "meeting_invited", topic: "周会"}, "📞 机器人被邀请加入会议：周会（未配置自动入会，仅通知）"},
		{meetingNote{kind: "meeting_started", topic: "评审"}, "📞 会议开始：评审"},
		{meetingNote{kind: "meeting_ended", topic: "评审"}, "📞 会议结束：评审"},
		{meetingNote{kind: "meeting_recording", topic: "评审", url: "https://r", duration: "3600000"}, "🎬 会议录制已就绪（时长 60 分钟）：评审\nhttps://r"},
	}
	for _, c := range cases {
		if got := formatMeeting(c.n); got != c.want {
			t.Errorf("formatMeeting(%+v)\n want %q\n got  %q", c.n, c.want, got)
		}
	}
	// unnamed meeting falls back; bad duration is skipped, url-less recording ok
	if got := formatMeeting(meetingNote{kind: "meeting_started"}); got != "📞 会议开始：未命名会议" {
		t.Errorf("fallback topic: %q", got)
	}
	if got := formatMeeting(meetingNote{kind: "meeting_recording", topic: "x", duration: "abc"}); got != "🎬 会议录制已就绪：x" {
		t.Errorf("bad duration: %q", got)
	}
	if got := formatMeeting(meetingNote{kind: "whatever"}); got != "" {
		t.Errorf("unknown kind should be empty: %q", got)
	}
}
