package cron

import (
	"testing"
	"time"
)

func TestNextBasics(t *testing.T) {
	cases := []struct {
		spec string
		now  string // RFC3339
		want string // RFC3339, next fire time
	}{
		// every minute
		{"* * * * *", "2026-11-19T10:30:15Z", "2026-11-19T10:31:00Z"},
		// daily at 09:30
		{"30 9 * * *", "2026-11-19T10:00:00Z", "2026-11-20T09:30:00Z"},
		{"30 9 * * *", "2026-11-19T08:00:00Z", "2026-11-19T09:30:00Z"},
		// step: every 5 minutes
		{"*/5 * * * *", "2026-11-19T10:32:00Z", "2026-11-19T10:35:00Z"},
		// range with step: hours 9-18 every 2h at :00
		{"0 9-18/2 * * *", "2026-11-19T10:00:00Z", "2026-11-19T11:00:00Z"},
		// list
		{"0 9,18 * * *", "2026-11-19T12:00:00Z", "2026-11-19T18:00:00Z"},
		// day of week: Monday (1) at 8:00
		{"0 8 * * 1", "2026-11-19T12:00:00Z", "2026-11-23T08:00:00Z"}, // Thu → next Mon
		// day of month + month: Nov 1 00:00
		{"0 0 1 11 *", "2026-11-19T12:00:00Z", "2027-11-01T00:00:00Z"},
		// exact now boundary: fires the NEXT minute, never the same one
		{"30 9 * * *", "2026-11-19T09:30:00Z", "2026-11-20T09:30:00Z"},
	}
	for _, c := range cases {
		now, _ := time.Parse(time.RFC3339, c.now)
		want, _ := time.Parse(time.RFC3339, c.want)
		got, err := Next(now, c.spec)
		if err != nil {
			t.Errorf("Next(%q): %v", c.spec, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("Next(%q, %s) = %s, want %s", c.spec, now, got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *", "a * * * *", "1-100 * * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}
