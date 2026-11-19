// Package fleet manages several chatmux instances from one CLI —
// botmux fleet's counterpart. A fleet.json lists named instances
// (baseUrl + adminToken); list/status/send/ask proxy each one.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// Instance is one chatmux gateway.
type Instance struct {
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
	Token   string `json:"token,omitempty"`
}

// Config is the fleet file.
type Config struct {
	Instances []Instance `json:"instances"`
}

// Load reads a fleet file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 fleet 文件 %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Get finds an instance by name.
func (c *Config) Get(name string) (*Instance, error) {
	for i := range c.Instances {
		if c.Instances[i].Name == name {
			return &c.Instances[i], nil
		}
	}
	return nil, fmt.Errorf("fleet 里没有实例 %s", name)
}

// Summary is one instance's health line.
type Summary struct {
	Name        string
	Reachable   bool
	Version     string
	ChannelsUp  int
	ChannelsAll int
	Agents      int
	Err         string
}

// Probe pings one instance's /admin/status.
func Probe(ctx context.Context, in Instance) Summary {
	s := Summary{Name: in.Name}
	body, code, err := req(ctx, in, http.MethodGet, "/admin/status", nil)
	if err != nil || code != 200 {
		if err == nil {
			err = fmt.Errorf("HTTP %d", code)
		}
		s.Err = err.Error()
		return s
	}
	s.Reachable = true
	var st struct {
		Version string          `json:"version"`
		Agents  int             `json:"agents"`
		Channel map[string]bool `json:"channels"`
	}
	_ = json.Unmarshal(body, &st)
	s.Version = st.Version
	s.Agents = st.Agents
	for _, ok := range st.Channel {
		s.ChannelsAll++
		if ok {
			s.ChannelsUp++
		}
	}
	return s
}

// List probes every instance concurrently, sorted by name.
func (c *Config) List(ctx context.Context) []Summary {
	out := make([]Summary, len(c.Instances))
	var wg sync.WaitGroup
	for i := range c.Instances {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			out[i] = Probe(cctx, c.Instances[i])
		}(i)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Call performs one admin request against a named instance and returns
// the raw body (client send/ask proxy through this).
func (c *Config) Call(ctx context.Context, name, method, path string, payload any) ([]byte, int, error) {
	in, err := c.Get(name)
	if err != nil {
		return nil, 0, err
	}
	return req(ctx, *in, method, path, payload)
}

func req(ctx context.Context, in Instance, method, path string, payload any) ([]byte, int, error) {
	var rd io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, in.BaseURL+path, rd)
	if err != nil {
		return nil, 0, err
	}
	if in.Token != "" {
		r.Header.Set("X-Admin-Token", in.Token)
	}
	if payload != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}
