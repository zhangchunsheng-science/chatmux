// Package cron is a minimal 5-field cron scheduler (min hour dom mon dow),
// enough for chatmux's config-driven scheduled prompts.
package cron

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is one scheduled job.
type Entry struct {
	Name string
	Spec string // "30 9 * * *"
	Fire func(ctx context.Context)
}

// Scheduler runs entries on their own goroutines.
type Scheduler struct {
	mu      sync.Mutex
	entries []Entry
}

func New() *Scheduler { return &Scheduler{} }

func (s *Scheduler) Add(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
}

// Start launches one goroutine per entry; all stop when ctx is done.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	entries := append([]Entry(nil), s.entries...)
	s.mu.Unlock()
	for _, e := range entries {
		go s.loop(ctx, e)
	}
}

func (s *Scheduler) loop(ctx context.Context, e Entry) {
	for {
		next, err := Next(time.Now(), e.Spec)
		if err != nil {
			return // bad spec: skip silently, config validation reports it
		}
		d := time.Until(next)
		if d < 0 {
			d = 0
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		safeFire(ctx, e)
	}
}

func safeFire(ctx context.Context, e Entry) {
	defer func() { _ = recover() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		e.Fire(ctx)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Minute):
	}
}

// field is one parsed cron field: a set of allowed values plus a step.
type field struct {
	set  map[int]bool
	step int
}

func (f field) match(v int) bool {
	if f.set == nil { // bare "*" — all values
		return v%f.step == 0
	}
	return f.set[v]
}

// ranges: [min,max] per field; dow/dom use 0 as wildcard after expansion.
var bounds = [5][2]int{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day of month
	{1, 12}, // month
	{0, 6},  // day of week
}

// Parse turns a 5-field spec into matchers.
func Parse(spec string) ([5]field, error) {
	var out [5]field
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return out, fmt.Errorf("cron: want 5 fields, got %d in %q", len(parts), spec)
	}
	for i, p := range parts {
		f, err := parseField(p, bounds[i][0], bounds[i][1])
		if err != nil {
			return out, fmt.Errorf("cron: field %d (%q): %w", i+1, p, err)
		}
		out[i] = f
	}
	return out, nil
}

func parseField(s string, min, max int) (field, error) {
	f := field{step: 1}
	if s == "*" || s == "*/1" {
		return f, nil
	}
	var listStr string
	var step int
	if i := strings.Index(s, "/"); i >= 0 {
		var err error
		step, err = strconv.Atoi(s[i+1:])
		if err != nil || step <= 0 {
			return f, fmt.Errorf("bad step %q", s[i+1:])
		}
		listStr = s[:i]
	} else {
		step = 1
		listStr = s
	}
	f.step = step
	f.set = map[int]bool{}
	for _, part := range strings.Split(listStr, ",") {
		lo, hi := min, max
		switch {
		case part == "*" || part == "":
			// full range with step
		case strings.Contains(part, "-"):
			bits := strings.SplitN(part, "-", 2)
			var err error
			lo, err = strconv.Atoi(bits[0])
			if err != nil {
				return f, fmt.Errorf("bad range %q", part)
			}
			hi, err = strconv.Atoi(bits[1])
			if err != nil {
				return f, fmt.Errorf("bad range %q", part)
			}
		default:
			v, err := strconv.Atoi(part)
			if err != nil {
				return f, fmt.Errorf("bad value %q", part)
			}
			lo, hi = v, v
		}
		if lo < min || hi > max || lo > hi {
			return f, fmt.Errorf("out of range [%d,%d]: %q", min, max, part)
		}
		for v := lo; v <= hi; v += step {
			f.set[v] = true
		}
	}
	return f, nil
}

// Next returns the next time after now matching the spec.
func Next(now time.Time, spec string) (time.Time, error) {
	f, err := Parse(spec)
	if err != nil {
		return time.Time{}, err
	}
	// Start at the next whole minute.
	t := now.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(2, 0, 0) // give up after 2 years (e.g. Feb 30)
	for t.Before(limit) {
		if f[0].match(t.Minute()) && f[1].match(t.Hour()) &&
			f[2].match(t.Day()) && f[3].match(int(t.Month())) && f[4].match(int(t.Weekday())) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron: no match within 2 years")
}
