package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// UnknownKeyWarnings reports config keys the JSON decoder silently
// drops. encoding/json ignores unrecognized field names without error,
// so "apikey" for "apiKey" or "lisen" for "listen" quietly disables a
// feature instead of failing boot. The expected keys are derived from
// the real Config structs via reflection — the schema cannot drift from
// the decoder's view. Channel options bodies are adapter-specific and
// skipped. Each finding names the JSON path and, when a near miss is
// found, the likely intended key.
func UnknownKeyWarnings(raw []byte) []string {
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil // Load already reported parse errors
	}
	var out []string
	walk(structSchema(reflect.TypeOf(Config{})), tree, "", &out)
	return out
}

var (
	rawMsgType = reflect.TypeOf(json.RawMessage{})
	timeType   = reflect.TypeOf(time.Time{})
)

// structSchema maps json field name → descriptor of that field's value:
// nil for scalars/opaque, map[string]any for objects, []any (one
// element, the item descriptor) for arrays.
func structSchema(t reflect.Type) map[string]any {
	m := map[string]any{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		name := f.Name
		if tag := f.Tag.Get("json"); tag != "" {
			first := strings.Split(tag, ",")[0]
			if first == "-" {
				continue
			}
			if first != "" {
				name = first
			}
		}
		m[name] = valueSchema(f.Type)
	}
	return m
}

func valueSchema(t reflect.Type) any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMsgType || t == timeType {
		return nil // opaque payload / serialized time
	}
	if t.Kind() == reflect.Struct {
		return structSchema(t)
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		return []any{valueSchema(t.Elem())}
	}
	return nil // scalar or map — leaves are opaque
}

func walk(desc any, node any, path string, out *[]string) {
	switch n := node.(type) {
	case map[string]any:
		fields, ok := desc.(map[string]any)
		if !ok {
			return // object where the schema has none — decoder's problem
		}
		for k, v := range n {
			sub, known := fields[k]
			if !known {
				p := strings.TrimPrefix(path+"."+k, ".")
				*out = append(*out, fmt.Sprintf("unknown key %q at %s%s", k, p, suggest(k, fields)))
				continue
			}
			walk(sub, v, path+"."+k, out)
		}
	case []any:
		arr, ok := desc.([]any)
		if !ok || len(arr) == 0 {
			return
		}
		for i, e := range n {
			walk(arr[0], e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// suggest finds the intended key for a typo: a case-insensitive exact
// match, or a close edit-distance match for reasonably long keys.
func suggest(typo string, fields map[string]any) string {
	known := make([]string, 0, len(fields))
	for k := range fields {
		known = append(known, k)
	}
	return Suggest(typo, known)
}

// Suggest returns "" or a " (did you mean ...?)" hint for typo against
// the known vocabulary — shared by config-key, provider and channel
// type checks.
func Suggest(typo string, known []string) string {
	best, bestDist := "", 3 // > max distance we'll suggest
	for _, k := range known {
		if strings.EqualFold(k, typo) {
			return fmt.Sprintf(" (did you mean %q? names are case-sensitive)", k)
		}
		if len(k) < 4 || len(typo) < 4 {
			continue
		}
		if d := levenshtein(typo, k); d < bestDist {
			best, bestDist = k, d
		}
	}
	if best != "" {
		return fmt.Sprintf(" (did you mean %q?)", best)
	}
	return ""
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
