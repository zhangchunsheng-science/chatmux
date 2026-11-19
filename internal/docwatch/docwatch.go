// Package docwatch watches local documents and fires a prompt into a
// chat when content changes — botmux doc-watch's file-based counterpart.
// First sighting only records the baseline; every later change produces
// a line diff excerpt fed to the agent via {{diff}}.
package docwatch

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zhangchunsheng-science/chatmux/internal/config"
)

// Fire is called once per detected change with the rendered prompt.
type Fire func(name, prompt string)

// Watcher polls every entry on its interval.
type Watcher struct {
	mu    sync.Mutex
	state map[string]string // name → last content ("" = not yet seen)

	entries []config.DocWatchConfig
	fire    Fire
}

func New(entries []config.DocWatchConfig, fire Fire) *Watcher {
	return &Watcher{state: map[string]string{}, entries: entries, fire: fire}
}

// Start polls until ctx is done. Entries sharing an interval tick
// together; minimum 5s to stay gentle on disk.
func (w *Watcher) Start(ctx context.Context) {
	tickers := map[int]*time.Ticker{}
	ints := map[int][]config.DocWatchConfig{}
	for _, e := range w.entries {
		iv := e.IntervalSec
		if iv < 5 {
			iv = 60
			if e.IntervalSec > 0 {
				iv = 5
			}
		}
		if _, ok := tickers[iv]; !ok {
			tickers[iv] = time.NewTicker(time.Duration(iv) * time.Second)
		}
		ints[iv] = append(ints[iv], e)
	}
	for iv, t := range tickers {
		go func(iv int, t *time.Ticker) {
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					for _, e := range ints[iv] {
						w.Poll(e)
					}
				}
			}
		}(iv, t)
	}
}

// Poll checks one entry once (exported for tests).
func (w *Watcher) Poll(e config.DocWatchConfig) {
	content, err := os.ReadFile(e.Path)
	if err != nil {
		return // missing/unreadable: skip silently, keep last state
	}
	cur := string(content)
	w.mu.Lock()
	prev, seen := w.state[e.Name]
	w.state[e.Name] = cur
	w.mu.Unlock()
	if !seen || cur == prev {
		return
	}
	prompt := e.Prompt
	if prompt == "" {
		prompt = "文档 {{name}} 有变更（{{path}}）：\n{{diff}}"
	}
	diff := DiffExcerpt(prev, cur, 60)
	prompt = strings.ReplaceAll(prompt, "{{diff}}", diff)
	prompt = strings.ReplaceAll(prompt, "{{path}}", e.Path)
	prompt = strings.ReplaceAll(prompt, "{{name}}", e.Name)
	log.Printf("docwatch %s changed (%d bytes → %d bytes)", e.Name, len(prev), len(cur))
	w.fire(e.Name, prompt)
}

// op is one line of a diff.
type op struct {
	kind byte // '+' | '-'
	line string
}

// diffLines computes an LCS line diff (DP, bounded to 4000 lines/side).
func diffLines(a, b []string) []op {
	if len(a) > 4000 {
		a = a[len(a)-4000:]
	}
	if len(b) > 4000 {
		b = b[len(b)-4000:]
	}
	n, m := len(a), len(b)
	// lcs[i][j] = LCS length of a[i:], b[j:]
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, op{'-', a[i]})
			i++
		default:
			out = append(out, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, op{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, op{'+', b[j]})
	}
	return out
}

// DiffExcerpt renders the changed lines (no context), capped at max
// lines, with a summary header.
func DiffExcerpt(oldContent, newContent string, max int) string {
	if max <= 0 {
		max = 60
	}
	ops := diffLines(strings.Split(oldContent, "\n"), strings.Split(newContent, "\n"))
	var add, del int
	for _, o := range ops {
		if o.kind == '+' {
			add++
		} else {
			del++
		}
	}
	var b strings.Builder
	b.WriteString(strings.Repeat("─", 20) + "\n")
	if add == 0 && del == 0 {
		b.WriteString("（无行级差异）\n")
		return b.String()
	}
	b.WriteString("+ 新增 / - 删除（最多展示前 " + itoa(max) + " 处）\n")
	shown := 0
	for _, o := range ops {
		if shown >= max {
			b.WriteString("…（其余省略）\n")
			break
		}
		b.WriteByte(o.kind)
		b.WriteByte(' ')
		b.WriteString(o.line)
		b.WriteByte('\n')
		shown++
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
