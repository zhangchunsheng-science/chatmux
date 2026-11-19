package agent

import (
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
