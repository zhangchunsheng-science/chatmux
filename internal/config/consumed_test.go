package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestConfigFieldsAreConsumed guards against dead config: every field
// declared on the config structs must be read somewhere outside the
// config package (definitions and tests excluded). The baseUrl bug —
// a documented agent field the providers silently ignored for months —
// is exactly the drift this catches at commit time.
//
// Generic identifiers (Name, Type, …) appear everywhere and prove
// nothing, so they're skipped; distinctive names must have at least
// one non-test consumer.
func TestConfigFieldsAreConsumed(t *testing.T) {
	skip := map[string]bool{
		"Name": true, "Type": true, "Model": true, "APIKey": true,
		"Provider": true, "HTTP": true, "Channels": true, "Agents": true,
		"Cron": true, "Listen": true, "Token": true, "Text": true,
		"Title": true, "ID": true, "URL": true, "Enabled": true,
	}
	src := repoSources(t)
	structs := []string{
		"Config", "AgentConfig", "ChannelConfig", "HTTPConfig",
		"CronConfig", "DocWatchConfig", "OncallConfig", "MeetingConfig",
		"WorkflowConfig", "FormConfig", "TranscribeConfig", "I18nConfig",
	}
	for _, name := range structs {
		for _, f := range structFields(name) {
			if skip[f] {
				continue
			}
			re := regexp.MustCompile(`\b` + regexp.QuoteMeta(f) + `\b`)
			found := false
			for _, s := range src {
				if re.MatchString(s.data) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s.%s is declared but never read outside the config package — dead config drifts (baseUrl bug class)", name, f)
			}
		}
	}
}

// structFields returns the exported field names of a config struct by
// Go name.
func structFields(name string) []string {
	types := map[string]reflect.Type{
		"Config":           reflect.TypeOf(Config{}),
		"AgentConfig":      reflect.TypeOf(AgentConfig{}),
		"ChannelConfig":    reflect.TypeOf(ChannelConfig{}),
		"HTTPConfig":       reflect.TypeOf(HTTPConfig{}),
		"CronConfig":       reflect.TypeOf(CronConfig{}),
		"DocWatchConfig":   reflect.TypeOf(DocWatchConfig{}),
		"OncallConfig":     reflect.TypeOf(OncallConfig{}),
		"MeetingConfig":    reflect.TypeOf(MeetingConfig{}),
		"WorkflowConfig":   reflect.TypeOf(WorkflowConfig{}),
		"FormConfig":       reflect.TypeOf(FormConfig{}),
		"TranscribeConfig": reflect.TypeOf(TranscribeConfig{}),
		"I18nConfig":       reflect.TypeOf(I18nConfig{}),
	}
	typ, ok := types[name]
	if !ok {
		return nil
	}
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		out = append(out, typ.Field(i).Name)
	}
	return out
}

type srcFile struct {
	path string
	data string
}

// repoSources loads non-test Go sources outside the config package and
// the desktop shell (separate app).
func repoSources(t *testing.T) []srcFile {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var out []srcFile
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "desktop", "config", "node_modules", "testdata":
				if path != root {
					return filepath.SkipDir
				}
			}
			if strings.HasPrefix(info.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, srcFile{path: path, data: string(b)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 30 {
		t.Fatalf("only %d sources found — walk is broken", len(out))
	}
	return out
}
