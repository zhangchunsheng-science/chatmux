package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepAttachments(t *testing.T) {
	dir := t.TempDir()
	old, fresh := filepath.Join(dir, "a.png"), filepath.Join(dir, "b.png")
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("yy"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	files, bytes := sweepAttachments(dir, 24*time.Hour)
	if files != 1 || bytes != 1 {
		t.Fatalf("sweep = %d files %d bytes, want 1/1", files, bytes)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("aged file survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh file was swept: %v", err)
	}
	// missing dir is a no-op, not an error
	if f, b := sweepAttachments(filepath.Join(dir, "nope"), time.Hour); f != 0 || b != 0 {
		t.Fatalf("missing dir sweep = %d/%d, want 0/0", f, b)
	}
}

// The loop must sweep at boot (leftovers from a long downtime are the
// main disk pressure), then keep ticking until cancelled.
func TestSweepLoopRunsAndStops(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "old.jpg")
	if err := os.WriteFile(stale, []byte("zzz"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sweepLoop(ctx, dir, 24*time.Hour, 50*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("boot sweep never removed the stale file")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sweepLoop did not exit on cancel")
	}
}
