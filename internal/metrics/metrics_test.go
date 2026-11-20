package metrics

import "testing"

func TestCounters(t *testing.T) {
	wd0, sr0, cr0 := Snapshot()
	WatchdogFire()
	WatchdogFire()
	StreamRetry()
	ChannelRestart()
	wd, sr, cr := Snapshot()
	if wd-wd0 != 2 {
		t.Fatalf("watchdog fires delta = %d, want 2", wd-wd0)
	}
	if sr-sr0 != 1 {
		t.Fatalf("stream retries delta = %d, want 1", sr-sr0)
	}
	if cr-cr0 != 1 {
		t.Fatalf("channel restarts delta = %d, want 1", cr-cr0)
	}
}
