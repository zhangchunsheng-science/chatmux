//go:build unix

package cliagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestCancelKillsProcessGroup pins the /stop and watchdog path: cancelling
// the turn must take down the CLI child AND its descendants. The script
// spawns a marked grandchild and then waits on it — without the group kill,
// the grandchild outlives the cancel (and holds the stdout pipe open).
func TestCancelKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available")
	}
	// defensive pre-clean: a grandchild leaked by an earlier (failed) run
	// of this very test would false-red the pgrep assertion below
	_ = exec.Command("pkill", "-f", "sleep 214214").Run()
	dir := t.TempDir()
	script := filepath.Join(dir, "slowcli.sh")
	// grandchild "sleep 214214" is uniquely greppable; the script echoes a
	// line then blocks in wait, mimicking a CLI babysitting a daemon child
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 214214 &\necho started\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRunner(dir)
	ctx, cancel := context.WithCancel(context.Background())
	events, err := r.Run(ctx, Config{Type: "generic", Bin: script}, "a1", "hi")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("events closed before the first line")
		}
		if ev.Text != "started\n" {
			t.Fatalf("first event = %q", ev.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no first event; script never ran")
	}

	cancel()
	// events must close (pipe reaped) and the grandchild must be gone
	closed := make(chan struct{})
	go func() {
		for range events {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("events channel did not close after cancel — pipe held open")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := exec.Command("pgrep", "-f", "sleep 214214").Run(); err != nil {
			return // no match: grandchild gone
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("grandchild survived the cancel — process group not killed")
}
