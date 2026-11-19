package agent

import (
	"testing"

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
