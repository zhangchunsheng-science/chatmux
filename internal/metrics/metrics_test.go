package metrics

import "testing"

func TestCounters(t *testing.T) {
	wd0, sr0 := Snapshot()
	WatchdogFire()
	WatchdogFire()
	StreamRetry()
	wd, sr := Snapshot()
	if wd-wd0 != 2 {
		t.Fatalf("watchdog fires delta = %d, want 2", wd-wd0)
	}
	if sr-sr0 != 1 {
		t.Fatalf("stream retries delta = %d, want 1", sr-sr0)
	}
}
