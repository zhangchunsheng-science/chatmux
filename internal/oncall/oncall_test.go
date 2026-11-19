package oncall

import (
	"strings"
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/config"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	tv, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return tv
}

func TestDailyRotation(t *testing.T) {
	s, err := Parse(config.OncallConfig{
		Members:  []string{"张三", "李四", "王五"},
		Anchor:   "2026-11-01",
		Rotation: "daily",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"2026-11-01 09:00": "张三",
		"2026-11-02 23:59": "李四",
		"2026-11-03 00:01": "王五",
		"2026-11-04 08:00": "张三", // wraps
	}
	for ts, want := range cases {
		if got := s.At(at(t, ts)); got != want {
			t.Errorf("%s: got %s want %s", ts, got, want)
		}
	}
	// before anchor clamps to first
	if got := s.At(at(t, "2026-10-20 09:00")); got != "张三" {
		t.Errorf("pre-anchor: %s", got)
	}
}

func TestWeeklyRotation(t *testing.T) {
	s, err := Parse(config.OncallConfig{
		Members:  []string{"A", "B"},
		Anchor:   "2026-11-02", // a Monday
		Rotation: "weekly",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// week 0: Mon 11-02 … Sun 11-08 → A; week 1: B
	if got := s.At(at(t, "2026-11-08 23:00")); got != "A" {
		t.Errorf("sun of week0: %s", got)
	}
	if got := s.At(at(t, "2026-11-09 00:01")); got != "B" {
		t.Errorf("mon of week1: %s", got)
	}
	end := s.ShiftEnd(at(t, "2026-11-11 10:00"))
	if end.Weekday() != time.Monday || end.Format("01-02") != "11-16" {
		t.Errorf("weekly shift end: %v", end)
	}
}

func TestShiftEndAndUpcoming(t *testing.T) {
	s, _ := Parse(config.OncallConfig{Members: []string{"A", "B", "C"}, Anchor: "2026-11-01"}, time.Now())
	end := s.ShiftEnd(at(t, "2026-11-01 15:00"))
	if end.Format("2006-01-02 15:04") != "2026-11-02 00:00" {
		t.Errorf("daily shift end: %v", end)
	}
	up := s.Upcoming(at(t, "2026-11-01 15:00"), 3)
	if strings.Join(up, ",") != "B,C,A" {
		t.Errorf("upcoming: %v", up)
	}
}

func TestHandoverText(t *testing.T) {
	s, _ := Parse(config.OncallConfig{Members: []string{"A", "B"}, Anchor: "2026-11-01"}, time.Now())
	// anchor day: yesterday clamps to the same first member → no notice
	if s.HandoverText(at(t, "2026-11-01 12:00")) != "" {
		t.Error("anchor day should be no handover")
	}
	// day after: member changed
	if ht := s.HandoverText(at(t, "2026-11-02 00:01")); !strings.Contains(ht, "A → B") {
		t.Errorf("handover text: %q", ht)
	}
	// later same day still reports the boundary fact
	if ht := s.HandoverText(at(t, "2026-11-03 12:00")); !strings.Contains(ht, "B → A") {
		t.Errorf("day-2 handover: %q", ht)
	}
}
