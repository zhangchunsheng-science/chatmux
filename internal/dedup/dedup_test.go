package dedup

import (
	"path/filepath"
	"testing"
	"time"
)

func TestClaimOnce(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "seen.json"), time.Hour, 100)
	if !s.Claim("m1") {
		t.Fatal("first claim must succeed")
	}
	if s.Claim("m1") {
		t.Fatal("second claim must be deduped")
	}
	if !s.Claim("m2") {
		t.Fatal("different id must succeed")
	}
}

func TestClaimPersistedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seen.json")
	s := New(path, time.Hour, 100)
	s.Claim("m1")
	s2 := New(path, time.Hour, 100)
	if s2.Claim("m1") {
		t.Fatal("dedup must survive restart")
	}
}

func TestClaimTTLExpiry(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "seen.json"), 50*time.Millisecond, 100)
	s.Claim("m1")
	time.Sleep(80 * time.Millisecond)
	if !s.Claim("m1") {
		t.Fatal("expired id must be claimable again")
	}
}

func TestClaimCapEviction(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "seen.json"), time.Hour, 3)
	for i := 0; i < 5; i++ {
		s.Claim(string(rune('a' + i)))
	}
	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	if n > 3 {
		t.Fatalf("cap not enforced: %d entries", n)
	}
}
