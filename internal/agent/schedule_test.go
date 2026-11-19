package agent

import (
	"testing"
)

func TestParseScheduleSpec(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"每天 9:30", "30 9 * * *"},
		{"9:30", "30 9 * * *"},
		{"每日 09:05", "5 9 * * *"},
		{"工作日 8:00", "0 8 * * 1-5"},
		{"每周三 10:00", "0 10 * * 3"},
		{"每周末 11:00", "0 11 * * 0,6"},
		{"每月1日 9:00", "0 9 1 * *"},
		{"每月15号 23:59", "59 23 15 * *"},
		{"*/10 * * * *", "*/10 * * * *"}, // raw cron passthrough
	} {
		got, ok := parseScheduleSpec(tc.in)
		if !ok || got != tc.want {
			t.Errorf("parseScheduleSpec(%q) = %q,%v want %q", tc.in, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"有空", "明天吧", "99:00", "每周八 9:00", "每月32日 9:00"} {
		if _, ok := parseScheduleSpec(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}
