package bus

import (
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/channel"
	"github.com/zhangchunsheng-science/chatmux/internal/metrics"
)

// waitFor polls cond until the deadline — the pump delivers
// asynchronously, so tests must not assume synchronous handoff.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBusFanoutBothSubscribersReceive(t *testing.T) {
	b := New()
	defer b.Close()
	a, c := b.Subscribe(), b.Subscribe()
	b.Inbound() <- channel.Message{ID: "m1"}
	b.Inbound() <- channel.Message{ID: "m2"}
	want := map[string]int{"a": 0, "c": 0}
	waitFor(t, 2*time.Second, func() bool {
		for len(a) > 0 {
			if msg := <-a; msg.ID == "m1" || msg.ID == "m2" {
				want["a"]++
			}
		}
		for len(c) > 0 {
			if msg := <-c; msg.ID == "m1" || msg.ID == "m2" {
				want["c"]++
			}
		}
		return want["a"] == 2 && want["c"] == 2
	})
	if want["a"] != 2 || want["c"] != 2 {
		t.Fatalf("fanout incomplete: %+v", want)
	}
}

func TestBusDropsSlowSubscriberAndCounts(t *testing.T) {
	// Publish is synchronous, so calling it directly with the fast
	// subscriber drained between calls makes the drop count exact:
	// slow's buffer holds 64, everything past that must drop+count.
	b := New()
	defer b.Close()
	fast, slow := b.Subscribe(), b.Subscribe()
	const n = 100
	fastGot := 0
	for i := 0; i < n; i++ {
		b.Publish(channel.Message{ID: "m"})
		for len(fast) > 0 {
			<-fast
			fastGot++
		}
	}
	if fastGot != n {
		t.Fatalf("fast subscriber lost messages: got %d of %d", fastGot, n)
	}
	if drops := metrics.BusDrops(); drops < n-64 {
		t.Fatalf("slow subscriber should force >= %d drops, counter says %d", n-64, drops)
	}
	_ = slow
}

func TestBusCloseDrainsThenClosesSubscribers(t *testing.T) {
	b := New()
	a := b.Subscribe()
	for i := 0; i < 10; i++ {
		b.Inbound() <- channel.Message{ID: "m"}
	}
	b.Close() // idempotency: a second Close must not panic
	b.Close()
	got := 0
	closed := false
	deadline := time.Now().Add(2 * time.Second)
	for !closed && time.Now().Before(deadline) {
		select {
		case _, ok := <-a:
			if ok {
				got++
			} else {
				closed = true
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !closed {
		t.Fatal("subscriber channel should close after Close drains")
	}
	if got != 10 {
		t.Fatalf("Close must deliver buffered messages, got %d of 10", got)
	}
}

func TestBusSubscribeAfterClose(t *testing.T) {
	b := New()
	b.Close()
	late := b.Subscribe()
	// once the pump has wound down, the late subscriber sees a closed
	// channel rather than attaching to a dead bus
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case _, ok := <-late:
			if !ok {
				return // closed: expected
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatal("late subscriber channel never closed")
}
