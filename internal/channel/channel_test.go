package channel

import (
	"errors"
	"strings"
	"testing"
)

func TestReadCapped(t *testing.T) {
	cases := []struct {
		name  string
		input string
		limit int
		ok    bool
	}{
		{"empty", "", 10, true},
		{"under", "hello", 10, true},
		{"exact-at-cap", "0123456789", 10, true},
		{"one-over", "0123456789A", 10, false},
		{"far-over", strings.Repeat("x", 100), 10, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := readCapped(strings.NewReader(c.input), c.limit)
			if c.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if string(b) != c.input {
					t.Fatalf("content mismatch: got %d bytes", len(b))
				}
			} else {
				if err == nil {
					t.Fatalf("expected cap error, got %d bytes", len(b))
				}
				if !strings.Contains(err.Error(), "cap") {
					t.Fatalf("error should mention the cap: %v", err)
				}
			}
		})
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadCappedReadError(t *testing.T) {
	if _, err := readCapped(errReader{}, 10); err == nil || err.Error() != "boom" {
		t.Fatalf("want underlying read error, got %v", err)
	}
}
