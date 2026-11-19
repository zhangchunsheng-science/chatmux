// Package oncall computes a duty rotation: members cycle daily or
// weekly from an anchor date — botmux oncall's deterministic take.
package oncall

import (
	"fmt"
	"strings"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/config"
)

// Schedule is a parsed rotation.
type Schedule struct {
	Members []string
	Weekly  bool
	Anchor  time.Time // local midnight of the anchor date
}

// Parse builds a Schedule (defaults: daily, anchor = today).
func Parse(c config.OncallConfig, now time.Time) (Schedule, error) {
	if len(c.Members) == 0 {
		return Schedule{}, fmt.Errorf("oncall: members is empty")
	}
	s := Schedule{Members: c.Members, Weekly: c.Rotation == "weekly"}
	s.Anchor = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if c.Anchor != "" {
		a, err := time.ParseInLocation("2006-01-02", c.Anchor, now.Location())
		if err != nil {
			return Schedule{}, fmt.Errorf("oncall: bad anchor %q: %w", c.Anchor, err)
		}
		s.Anchor = a
	}
	return s, nil
}

// slotAt maps a time to a rotation slot index (days or ISO weeks since
// the anchor; before the anchor it clamps to 0).
func (s Schedule) slotAt(t time.Time) int {
	d := t.Sub(s.Anchor)
	if d < 0 {
		return 0
	}
	if s.Weekly {
		// ISO week boundaries: Monday 00:00
		days := int(d / (24 * time.Hour))
		return days / 7
	}
	return int(d / (24 * time.Hour))
}

// At returns the member on duty at t.
func (s Schedule) At(t time.Time) string {
	return s.Members[s.slotAt(t)%len(s.Members)]
}

// ShiftEnd returns when the current shift ends (next midnight or the
// next Monday midnight).
func (s Schedule) ShiftEnd(t time.Time) time.Time {
	mid := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	if s.Weekly {
		// days until next Monday (Mon→7, Tue→6, … Sun→1)
		untilMon := (8 - int(t.Weekday())) % 7
		if untilMon == 0 {
			untilMon = 7
		}
		return mid.AddDate(0, 0, untilMon)
	}
	return mid.AddDate(0, 0, 1)
}

// Upcoming lists the next n members after t's shift.
func (s Schedule) Upcoming(t time.Time, n int) []string {
	out := make([]string, 0, n)
	slot := s.slotAt(t)
	for i := 1; i <= n; i++ {
		out = append(out, s.Members[(slot+i)%len(s.Members)])
	}
	return out
}

// HandoverText compares yesterday's member with today's and returns a
// notice ("" when the same member continues — e.g. single-member or the
// same slot after a clamp).
func (s Schedule) HandoverText(now time.Time) string {
	prev := s.At(now.AddDate(0, 0, -1))
	cur := s.At(now)
	if prev == cur {
		return ""
	}
	return fmt.Sprintf("🔔 值班交接：%s → %s", prev, cur)
}

// Roster renders the rotation order for listings.
func (s Schedule) Roster() string {
	return strings.Join(s.Members, " → ")
}
