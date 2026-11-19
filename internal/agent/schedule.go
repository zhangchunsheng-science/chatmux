// /schedule: runtime-managed cron schedules persisted under DataDir —
// chatmux's deterministic take on botmux's natural-language reminders.
// Grammar: 每天/工作日/每周X/每周末/每月N日 + HH:MM, or a raw 5-field spec.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/cron"
	"github.com/zhangchunsheng-science/chatmux/internal/session"
)

// userSchedule is one chat-created schedule, persisted across restarts.
type userSchedule struct {
	ID       string `json:"id"`
	Spec     string `json:"spec"` // cron spec, e.g. "30 9 * * 1-5"
	When     string `json:"when"` // original phrase, for listing
	Channel  string `json:"channel"`
	ChatID   string `json:"chatId"`
	ThreadID string `json:"threadId,omitempty"`
	Text     string `json:"text"`
}

// SetScheduler attaches the live scheduler; stored user schedules are
// re-registered on boot.
func (r *Runtime) SetScheduler(s *cron.Scheduler, ctx context.Context) {
	r.sched = s
	r.schedCtx = ctx
	r.loadSchedules()
	for i := range r.userScheds {
		us := r.userScheds[i]
		s.AddLive(ctx, r.userEntry(us))
	}
}

func (r *Runtime) userEntry(us userSchedule) cron.Entry {
	return cron.Entry{
		Name: us.ID,
		Spec: us.Spec,
		Fire: func(ctx context.Context) {
			r.Handle(ctx, channel.Message{
				Channel: us.Channel, ChatID: us.ChatID, ThreadID: us.ThreadID,
				ChatType: "p2p", MentionedMe: true,
				UserName: "schedule:" + us.When, Text: us.Text,
				Timestamp: time.Now(),
			})
		},
	}
}

func (r *Runtime) schedPath() string {
	dir := r.dataDir
	if dir == "" {
		dir = "data"
	}
	return filepath.Join(dir, "schedules.json")
}

func (r *Runtime) loadSchedules() {
	b, err := os.ReadFile(r.schedPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &r.userScheds)
}

func (r *Runtime) saveSchedules() {
	b, err := json.Marshal(r.userScheds)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(r.schedPath()), 0o755)
	tmp := r.schedPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, r.schedPath())
	}
}

// handleSchedule implements /schedule [add <when> <text> | del <id> | list].
func (r *Runtime) handleSchedule(key session.Key, msg channel.Message, reply func(string)) {
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.Text), "/schedule"))
	sub, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	switch sub {
	case "", "list":
		if len(r.userScheds) == 0 {
			reply(r.say("sched.none"))
			return
		}
		lines := []string{r.say("sched.header")}
		for _, us := range r.userScheds {
			lines = append(lines, r.say("sched.line", us.ID, us.When, truncate(us.Text, 40)))
		}
		reply(strings.Join(lines, "\n"))
	case "del", "rm":
		if rest == "" {
			reply(r.say("sched.del.usage"))
			return
		}
		if r.sched == nil || r.sched.Remove(rest) == 0 {
			reply(r.say("sched.del.none", rest))
			return
		}
		out := r.userScheds[:0]
		for _, us := range r.userScheds {
			if us.ID != rest {
				out = append(out, us)
			}
		}
		r.userScheds = out
		r.saveSchedules()
		reply(r.say("sched.del.ok", rest))
	case "add":
		when, text, _ := strings.Cut(rest, " ")
		text = strings.TrimSpace(text)
		spec, ok := parseScheduleSpec(when)
		if !ok {
			reply(r.say("sched.bad", when))
			return
		}
		if text == "" {
			reply(r.say("sched.add.usage"))
			return
		}
		max := 0
		for _, us := range r.userScheds {
			if n, err := strconv.Atoi(strings.TrimPrefix(us.ID, "s")); err == nil && n > max {
				max = n
			}
		}
		id := fmt.Sprintf("s%d", max+1)
		us := userSchedule{ID: id, Spec: spec, When: when,
			Channel: key.Channel, ChatID: key.ChatID, ThreadID: key.ThreadID, Text: text}
		r.userScheds = append(r.userScheds, us)
		r.saveSchedules()
		if r.sched != nil && r.schedCtx != nil {
			r.sched.AddLive(r.schedCtx, r.userEntry(us))
		}
		if next, err := cron.Next(time.Now(), spec); err == nil {
			reply(r.say("sched.add.ok", id, when, next.Format("01-02 15:04")))
		} else {
			reply(r.say("sched.add.ok", id, when, "?"))
		}
	default:
		reply(r.say("sched.usage"))
	}
}

// parseScheduleSpec maps a compact deterministic Chinese (or raw cron)
// time phrase to a 5-field spec.
func parseScheduleSpec(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if _, err := cron.Parse(s); err == nil { // raw spec passthrough
		return s, true
	}
	fields := strings.Fields(s)
	last := fields[len(fields)-1]
	hh, mm, ok := parseHHMM(last)
	if !ok {
		return "", false
	}
	head := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), last))
	switch {
	case head == "" || head == "每天" || head == "每日" || head == "daily":
		return fmt.Sprintf("%d %d * * *", mm, hh), true
	case head == "工作日" || head == "工作日早" || head == "weekdays":
		return fmt.Sprintf("%d %d * * 1-5", mm, hh), true
	case head == "周末" || head == "每周末" || head == "weekend":
		return fmt.Sprintf("%d %d * * 0,6", mm, hh), true
	case strings.HasPrefix(head, "每周"):
		dow := chineseDow(strings.TrimPrefix(head, "每周"))
		if dow < 0 {
			return "", false
		}
		return fmt.Sprintf("%d %d * * %d", mm, hh, dow), true
	case strings.HasPrefix(head, "每月"):
		day := strings.TrimSuffix(strings.TrimPrefix(head, "每月"), "日")
		day = strings.TrimSuffix(day, "号")
		n, err := strconv.Atoi(day)
		if err != nil || n < 1 || n > 31 {
			return "", false
		}
		return fmt.Sprintf("%d %d %d * *", mm, hh, n), true
	}
	return "", false
}

func parseHHMM(s string) (int, int, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

func chineseDow(s string) int {
	switch strings.TrimSpace(s) {
	case "一", "1":
		return 1
	case "二", "2":
		return 2
	case "三", "3":
		return 3
	case "四", "4":
		return 4
	case "五", "5":
		return 5
	case "六", "6":
		return 6
	case "日", "天", "末", "0", "7":
		return 0
	}
	return -1
}
