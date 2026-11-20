package agent

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/config"
)

func TestSayLocale(t *testing.T) {
	zhRT := &Runtime{cfg: &config.Config{}}
	if got := zhRT.say("stop.bg.none"); got != "没有在跑的后台任务。" {
		t.Errorf("zh default: %q", got)
	}
	enRT := &Runtime{cfg: &config.Config{I18n: &config.I18nConfig{Locale: "en"}}}
	if got := enRT.say("stop.bg.none"); got != "No background jobs running." {
		t.Errorf("en: %q", got)
	}
	if got := enRT.say("stop.bg.done", 3); got != "Stopped 3 background job(s)." {
		t.Errorf("en with args: %q", got)
	}
	// unknown locale falls back zh; unknown key falls back zh dict
	frRT := &Runtime{cfg: &config.Config{I18n: &config.I18nConfig{Locale: "fr"}}}
	if got := frRT.say("stop.bg.none"); got != "没有在跑的后台任务。" {
		t.Errorf("fr fallback: %q", got)
	}
	if got := enRT.say("no.such.key"); got != "no.such.key" {
		t.Errorf("missing key should pass through, got %q", got)
	}
}

// TestMessagesParity: every key must exist in both locales — a key
// missing from en silently serves Chinese to English users (and vice
// versa via the zh fallback).
func TestMessagesParity(t *testing.T) {
	for k := range zh {
		if _, ok := en[k]; !ok {
			t.Errorf("key %q missing from en dict", k)
		}
	}
	for k := range en {
		if _, ok := zh[k]; !ok {
			t.Errorf("key %q missing from zh dict", k)
		}
	}
}

// TestSayKeysDefined: every literal .say("key") call in this package
// must resolve to a real message — an undefined key replies with the
// raw key name (e.g. "sched.usage") instead of help text. Scans the
// package sources at test time (cwd is the package dir).
func TestSayKeysDefined(t *testing.T) {
	re := regexp.MustCompile(`\.say\("([a-z0-9.]+)"`)
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			calls++
			if _, ok := zh[m[1]]; !ok {
				t.Errorf("%s: say(%q) has no message defined", name, m[1])
			}
		}
	}
	if calls < 90 { // sanity: the scan must actually find the call sites
		t.Fatalf("scan found only %d say() calls — source layout changed?", calls)
	}
}

func TestParseWhen(t *testing.T) {
	// relative
	if at, ok := parseWhen("+30m"); !ok || at.Before(time.Now().Add(29*time.Minute)) {
		t.Errorf("+30m → %v ok=%v", at, ok)
	}
	// absolute HH:MM — always in the future
	at, ok := parseWhen("00:05")
	if !ok || !at.After(time.Now()) {
		t.Errorf("00:05 → %v ok=%v (must roll to tomorrow)", at, ok)
	}
	// full date
	if at, ok := parseWhen("2099-01-02 03:04"); !ok || at.Hour() != 3 || at.Minute() != 4 {
		t.Errorf("2099-01-02 03:04 → %v ok=%v", at, ok)
	}
	if _, ok := parseWhen("not-a-time"); ok {
		t.Error("garbage should not parse")
	}
	if _, ok := parseWhen("-30m"); ok {
		t.Error("negative duration should not parse")
	}
}

func TestHumanDur(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{35 * time.Minute, "35m"},
		{2*time.Hour + 5*time.Minute, "2h5m"},
		{26 * time.Hour, "1d2h"},
	} {
		if got := humanDur(tc.d); got != tc.want {
			t.Errorf("humanDur(%v) = %q want %q", tc.d, got, tc.want)
		}
	}
}
