package atomicfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The fixed-tmp-name bug this package exists for: concurrent writers
// interleaved bytes in one temp file. With unique temps, every observer
// of the target path must always see a complete, valid document.
func TestConcurrentWritesNeverTear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := Write(path, []byte(`{"n":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 200; i++ {
				b, _ := json.Marshal(map[string]int{"n": w*1000 + i})
				if err := Write(path, b, 0o600); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(w)
	}
	// reader runs until the writers finish; every read must unmarshal
	// cleanly (torn bytes = instant failure)
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			b, err := os.ReadFile(path)
			if err == nil {
				var m map[string]int
				if json.Unmarshal(b, &m) != nil {
					t.Errorf("torn read: %q", b)
					return
				}
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	writers.Wait()
	close(stop)
	select {
	case <-readerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("reader did not stop after writers finished")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", info.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("leftover temp files: %v", names)
	}
}
