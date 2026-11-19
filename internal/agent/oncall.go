// /oncall: show the duty rotation from config (current member, shift
// end, order, upcoming) — chatmux's take on botmux oncall.
package agent

import (
	"strings"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/oncall"
)

// handleOncallCommand renders /oncall [/next].
func (r *Runtime) handleOncallCommand(reply func(string), sub string) {
	oc := r.cfg.Oncall
	if oc == nil || len(oc.Members) == 0 {
		reply(r.say("oncall.none"))
		return
	}
	s, err := oncall.Parse(*oc, time.Now())
	if err != nil {
		reply(err.Error())
		return
	}
	now := time.Now()
	if sub == "next" {
		lines := []string{r.say("oncall.next.header")}
		up := s.Upcoming(now, 3)
		period := 1
		if s.Weekly {
			period = 7
		}
		from := s.ShiftEnd(now)
		for _, m := range up {
			to := from.AddDate(0, 0, period)
			lines = append(lines, r.say("oncall.next.line", m, from.Format("01-02"), to.Format("01-02")))
			from = to
		}
		reply(strings.Join(lines, "\n"))
		return
	}
	reply(r.say("oncall.now", s.At(now), s.ShiftEnd(now).Format("01-02 15:04"), s.Roster()))
}
