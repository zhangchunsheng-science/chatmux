package config

import (
	"strings"
	"testing"
)

func unknownIn(t *testing.T, raw string) []string {
	t.Helper()
	return UnknownKeyWarnings([]byte(raw))
}

func joined(ws []string) string { return strings.Join(ws, "\n") }

func TestUnknownKeysValidConfigSilent(t *testing.T) {
	// every documented section, including adapter-opaque channel
	// options — nothing here may be flagged
	raw := `{
	  "channels": [
	    {"type":"feishu","name":"fs","options":{"appId":"a","appSecret":"s","whateverElse":"x"}}
	  ],
	  "agents": [
	    {"name":"a","provider":"openai","apiKey":"k","turnTimeout":"45m",
	     "routes":[{"channel":"fs","chatId":"c1"}]},
	    {"name":"c","cli":{"type":"generic","bin":"/bin/echo"},"workingDir":"/tmp"}
	  ],
	  "defaultAgent":"a",
	  "http":{"listen":"127.0.0.1:18097","adminToken":"t"},
	  "cron":[{"name":"daily","schedule":"30 9 * * *","channel":"fs","chatId":"c1","text":"早","threadId":"t1"}],
	  "i18n":{"locale":"en"},
	  "dataDir":"data"
	}`
	if ws := unknownIn(t, raw); len(ws) != 0 {
		t.Fatalf("valid config flagged:\n%s", joined(ws))
	}
}

func TestUnknownKeysTypoSuggestions(t *testing.T) {
	raw := `{
	  "defaultAgent":"a",
	  "agents":[{"name":"a","apikey":"k"}],
	  "http":{"lisen":"127.0.0.1:1"}
	}`
	ws := unknownIn(t, raw)
	all := joined(ws)
	for _, want := range []string{`apikey`, `apiKey`, `lisen`, `listen`, `agents[0]`, `http`} {
		if !strings.Contains(all, want) {
			t.Fatalf("findings missing %q:\n%s", want, all)
		}
	}
	if len(ws) != 2 {
		t.Fatalf("want exactly 2 findings, got %d:\n%s", len(ws), all)
	}
}

func TestUnknownKeysCaseSensitivityHint(t *testing.T) {
	// same letters, wrong case: EqualFold match gets the explicit
	// case-sensitivity hint
	ws := unknownIn(t, `{"agents":[{"name":"a","apikey":"k"}]}`)
	all := joined(ws)
	if !strings.Contains(all, "case-sensitive") {
		t.Fatalf("case-insensitive typo should explain case sensitivity:\n%s", all)
	}
}

func TestUnknownKeysTopLevelAndNested(t *testing.T) {
	raw := `{
	  "agnts": [],
	  "docwatch": [{"name":"w","path":"/tmp/x","channel":"web","chatId":"c","prompt":"p","intervalm":"1m"}]
	}`
	ws := unknownIn(t, raw)
	all := joined(ws)
	if !strings.Contains(all, `"agnts"`) {
		t.Fatalf("top-level typo not reported:\n%s", all)
	}
	if !strings.Contains(all, "docwatch[0]") || !strings.Contains(all, "intervalm") {
		t.Fatalf("nested typo not reported with path:\n%s", all)
	}
}

func TestUnknownKeysNotScalarNoise(t *testing.T) {
	// values with scalar schemas must not produce findings even when
	// the JSON value type is odd (decoder behavior is out of scope)
	if ws := unknownIn(t, `{"dataDir":"d","defaultAgent":"a"}`); len(ws) != 0 {
		t.Fatalf("scalar keys flagged:\n%s", joined(ws))
	}
}
