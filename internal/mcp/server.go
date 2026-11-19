// Package mcp exposes a running chatmux as MCP tools over stdio
// (JSON-RPC 2.0, line-delimited): Claude Code and other MCP clients can
// chatmux send / ask / status / history — the surface botmux offers its
// host agents. Implemented against the admin HTTP API, no server state.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const protocolVersion = "2024-11-05"

// Server answers MCP requests by proxying a chatmux admin endpoint.
type Server struct {
	base  string // e.g. http://127.0.0.1:8080
	token string // adminToken ("" if none)
}

func New(base, token string) *Server { return &Server{base: base, token: token} }

// Serve reads line-delimited JSON-RPC requests from in and writes
// responses to out until EOF or ctx is done. Notifications get no reply.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	w := bufio.NewWriter(out)
	defer w.Flush()
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if resp := s.handle(ctx, line); resp != nil {
			b, err := json.Marshal(resp)
			if err != nil {
				continue
			}
			w.Write(b)
			w.WriteByte('\n')
			w.Flush()
		}
	}
	return sc.Err()
}

// rpc frames --------------------------------------------------------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handle(ctx context.Context, line string) *response {
	var req request
	if err := json.Unmarshal([]byte(line), &req); err != nil || req.Method == "" {
		return &response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32700, Message: "parse error"}}
	}
	if len(req.ID) == 0 { // notification — never reply
		return nil
	}
	result, rerr := s.dispatch(ctx, req.Method, req.Params)
	resp := &response{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
		return resp
	}
	resp.Result = result
	return resp
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *rpcError) {
	switch method {
	case "initialize":
		return json.RawMessage(`{"protocolVersion":"` + protocolVersion + `","capabilities":{"tools":{}},"serverInfo":{"name":"chatmux","version":"0.4.3"}}`), nil
	case "ping":
		return json.RawMessage(`{}`), nil
	case "tools/list":
		b, _ := json.Marshal(map[string]any{"tools": tools()})
		return b, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		text, isErr := s.callTool(ctx, p.Name, p.Arguments)
		b, _ := json.Marshal(map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
			"isError": isErr,
		})
		return b, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
	}
}

// tools --------------------------------------------------------------

type property struct {
	Type        string    `json:"type"`
	Description string    `json:"description,omitempty"`
	Items       *property `json:"items,omitempty"`
}

func prop(desc, typ string) property { return property{Type: typ, Description: desc} }

func tools() []map[string]any {
	strs := func() property { return property{Type: "array", Items: &property{Type: "string"}} }
	return []map[string]any{
		{
			"name":        "chatmux_send",
			"description": "Send a message (optionally with a local file/image) into an IM chat via a running chatmux gateway.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]property{
					"channel":  prop("channel instance name from chatmux.json, e.g. feishu / tg1", "string"),
					"chatId":   prop("target chat id (open_chat_id / chat id / channel id)", "string"),
					"threadId": prop("optional thread/topic id", "string"),
					"text":     prop("message text (caption when file is set)", "string"),
					"file":     prop("optional local file path to upload; images render inline", "string"),
				},
				"required": []string{"channel", "chatId", "text"},
			},
		},
		{
			"name":        "chatmux_ask",
			"description": "Ask the humans in a chat a blocking question and return their answer (options → form card; timeout default 600s).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]property{
					"channel":    prop("channel instance name", "string"),
					"chatId":     prop("target chat id", "string"),
					"question":   prop("question text shown to humans", "string"),
					"options":    func() property { p := strs(); p.Description = "fixed choices (recommended on feishu)"; return p }(),
					"timeoutSec": prop("wait timeout in seconds (default 600)", "number"),
				},
				"required": []string{"channel", "chatId", "question"},
			},
		},
		{
			"name":        "chatmux_status",
			"description": "Gateway health snapshot: channels, runtime counters, agents, cron entries.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]property{}},
		},
		{
			"name":        "chatmux_history",
			"description": "Recent stored conversation turns of one chat (newest last).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]property{
					"channel":  prop("channel instance name", "string"),
					"chatId":   prop("chat id", "string"),
					"threadId": prop("optional thread/topic id", "string"),
					"limit":    prop("max messages returned (default 20)", "number"),
				},
				"required": []string{"channel", "chatId"},
			},
		},
	}
}

// tool calls ---------------------------------------------------------

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	str := func(k string) string {
		if v, ok := a[k].(string); ok {
			return v
		}
		return ""
	}
	switch name {
	case "chatmux_send":
		payload := map[string]any{"channel": str("channel"), "chatId": str("chatId"), "text": str("text")}
		if f := str("file"); f != "" {
			payload["file"] = f
		}
		if t := str("threadId"); t != "" {
			payload["threadId"] = t
		}
		body, code, err := s.req(ctx, http.MethodPost, "/admin/send", payload)
		if err != nil {
			return "发送失败: " + err.Error(), true
		}
		if code != 200 {
			return fmt.Sprintf("发送失败 HTTP %d: %s", code, body), true
		}
		return "已发送 ✓", false
	case "chatmux_ask":
		payload := map[string]any{"channel": str("channel"), "chatId": str("chatId"), "question": str("question"), "timeoutSec": 600}
		if opts, ok := a["options"].([]any); ok && len(opts) > 0 {
			list := make([]string, 0, len(opts))
			for _, o := range opts {
				if t, ok := o.(string); ok {
					list = append(list, t)
				}
			}
			payload["options"] = list
		}
		if n, ok := a["timeoutSec"].(float64); ok && n > 0 {
			payload["timeoutSec"] = int(n)
		}
		body, code, err := s.req(ctx, http.MethodPost, "/admin/ask", payload)
		if err != nil {
			return "提问失败: " + err.Error(), true
		}
		if code == 504 {
			return "等待回答超时，未获答案。", true
		}
		if code != 200 {
			return fmt.Sprintf("提问失败 HTTP %d: %s", code, body), true
		}
		var out struct {
			Answer string `json:"answer"`
		}
		_ = json.Unmarshal(body, &out)
		return out.Answer, false
	case "chatmux_status":
		body, code, err := s.req(ctx, http.MethodGet, "/admin/status", nil)
		if err != nil {
			return "查询失败: " + err.Error(), true
		}
		if code != 200 {
			return fmt.Sprintf("查询失败 HTTP %d: %s", code, body), true
		}
		return string(body), false
	case "chatmux_history":
		q := "?channel=" + str("channel") + "&chatId=" + str("chatId")
		if t := str("threadId"); t != "" {
			q += "&threadId=" + t
		}
		limit := 20
		if n, ok := a["limit"].(float64); ok && n > 0 {
			limit = int(n)
		}
		q += fmt.Sprintf("&limit=%d", limit)
		body, code, err := s.req(ctx, http.MethodGet, "/admin/history"+q, nil)
		if err != nil {
			return "查询失败: " + err.Error(), true
		}
		if code != 200 {
			return fmt.Sprintf("查询失败 HTTP %d: %s", code, body), true
		}
		return string(body), false
	}
	return "unknown tool: " + name, true
}

func (s *Server) req(ctx context.Context, method, path string, payload any) ([]byte, int, error) {
	var rd io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, rd)
	if err != nil {
		return nil, 0, err
	}
	if s.token != "" {
		req.Header.Set("X-Admin-Token", s.token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}
