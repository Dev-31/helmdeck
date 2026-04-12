// Package pwmcp is a minimal client for the streamable-HTTP transport
// of @playwright/mcp (T807a / T807e / ADR 035).
//
// It is NOT a full MCP client — only the subset web.test needs:
//
//   - Initialize: POST the `initialize` handshake and capture the
//     `Mcp-Session-Id` header the server returns; subsequent calls
//     echo it back so Playwright MCP routes us to the right browser
//     context.
//   - ToolsCall: POST a `tools/call` request and return the text
//     payload from the first content block. Playwright MCP returns
//     either an updated accessibility snapshot or a generated
//     Playwright code stub — both are opaque strings from the pack's
//     perspective, so we do not parse them further.
//
// The real /api/v1/mcp/servers registry (internal/mcp) is about
// operator-configured external MCP servers; this package is scoped
// to the per-session @playwright/mcp child the sidecar auto-launches
// and deliberately does not touch that registry.
package pwmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	// DefaultTimeout is the per-request ceiling for Playwright MCP calls.
	// Navigation + snapshot against a real page can take several seconds
	// on cold Chromium; 30s is enough headroom without letting a hung
	// browser wedge the pack.
	DefaultTimeout = 30 * time.Second

	// protocolVersion matches the MCP streamable-HTTP revision @playwright/mcp
	// ships in its `--port` standalone mode at the time of T807e.
	protocolVersion = "2025-03-26"
)

// Client is a minimal streamable-HTTP MCP client bound to a single
// Playwright MCP endpoint. Zero value is not usable — call New.
type Client struct {
	endpoint  string // full URL, e.g. http://172.18.0.4:8931/mcp
	http      *http.Client
	sessionID atomic.Value // string — set by Initialize from the Mcp-Session-Id response header
	nextID    atomic.Int64
}

// New constructs a Client pointing at endpoint. http may be nil to use
// a default client with a reasonable per-request timeout.
func New(endpoint string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	c := &Client{endpoint: endpoint, http: httpClient}
	c.sessionID.Store("")
	return c
}

// Endpoint returns the configured Playwright MCP URL.
func (c *Client) Endpoint() string { return c.endpoint }

// rpcRequest and rpcResponse are the narrow JSON-RPC envelopes this
// client speaks. We skip the full MCP schema because Playwright MCP's
// streamable-HTTP surface is small enough to address directly.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Initialize performs the MCP `initialize` handshake and persists the
// session id from the Mcp-Session-Id response header. Safe to call
// more than once — subsequent calls reset the session.
func (c *Client) Initialize(ctx context.Context) error {
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "helmdeck-pwmcp",
			"version": "0.6.0",
		},
	})
	_, headers, err := c.rpc(ctx, "initialize", params)
	if err != nil {
		return err
	}
	if sid := headers.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID.Store(sid)
	}
	// Fire-and-forget `notifications/initialized` — spec says the
	// client sends this after the initialize reply to move into the
	// operational phase. Servers that don't care ignore it.
	_, _, _ = c.rpc(ctx, "notifications/initialized", nil)
	return nil
}

// ToolResult is the decoded payload of a Playwright MCP tool call.
// Playwright MCP returns content blocks in the MCP standard shape:
//
//	{"content": [{"type": "text", "text": "..."}]}
//
// For browser_snapshot the text block is the accessibility tree; for
// browser_navigate it is the generated Playwright code stub. We fold
// every text block into Text with newlines between them; Raw holds
// the original content array so callers with unusual needs (e.g.
// binary screenshots in a future iteration) can reach into it.
type ToolResult struct {
	Text    string          `json:"text"`
	IsError bool            `json:"is_error"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}

// toolsCallResponse matches the `tools/call` result shape.
type toolsCallResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"content"`
	IsError bool `json:"isError,omitempty"`
}

// ToolsCall invokes a Playwright MCP tool by name and returns the
// decoded result. Arguments MUST be a JSON object (or nil for tools
// that take no arguments).
func (c *Client) ToolsCall(ctx context.Context, tool string, arguments map[string]any) (*ToolResult, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	params, err := json.Marshal(map[string]any{
		"name":      tool,
		"arguments": arguments,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal tools/call params: %w", err)
	}
	raw, _, err := c.rpc(ctx, "tools/call", params)
	if err != nil {
		return nil, err
	}
	var resp toolsCallResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode tools/call result: %w", err)
	}
	var buf bytes.Buffer
	for i, block := range resp.Content {
		if block.Type != "text" {
			continue
		}
		if i > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(block.Text)
	}
	return &ToolResult{
		Text:    buf.String(),
		IsError: resp.IsError,
		Raw:     raw,
	}, nil
}

// rpc posts one JSON-RPC request and returns the Result bytes plus
// response headers (callers need the Mcp-Session-Id header from the
// initialize response). An RPC-level Error becomes a non-nil error.
func (c *Client) rpc(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, http.Header, error) {
	id := c.nextID.Add(1)
	// Notifications (method starts with "notifications/") have no id
	// per the JSON-RPC spec. We keep id=0 in that case and omit it
	// from the wire via the omitempty on the struct tag.
	if startsWith(method, "notifications/") {
		id = 0
	}
	reqBody := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	encoded, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal %s request: %w", method, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, nil, fmt.Errorf("build %s request: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Streamable HTTP transport: advertise both JSON and SSE as the
	// server may return either. Playwright MCP's single-shot tool
	// calls come back as application/json; long-running ones would
	// upgrade to text/event-stream, which a future iteration can
	// handle. For now we accept what we can parse.
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sid, _ := c.sessionID.Load().(string); sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("%s request: %w", method, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return nil, nil, fmt.Errorf("%s read body: %w", method, err)
	}

	if resp.StatusCode >= 500 {
		return nil, resp.Header, fmt.Errorf("%s: upstream %d: %s", method, resp.StatusCode, truncate(string(body), 256))
	}
	if resp.StatusCode >= 400 {
		return nil, resp.Header, fmt.Errorf("%s: upstream %d: %s", method, resp.StatusCode, truncate(string(body), 256))
	}
	if id == 0 {
		// Notification — no response body expected, don't try to decode.
		return nil, resp.Header, nil
	}

	// The streamable-HTTP transport can return a JSON body directly
	// OR an SSE stream containing one or more `message` events, each
	// carrying a JSON-RPC frame. Detect by Content-Type and decode
	// accordingly.
	ctype := resp.Header.Get("Content-Type")
	var rpcResp rpcResponse
	if startsWith(ctype, "text/event-stream") {
		frame, err := firstSSEDataFrame(body)
		if err != nil {
			return nil, resp.Header, fmt.Errorf("%s parse SSE: %w", method, err)
		}
		if err := json.Unmarshal(frame, &rpcResp); err != nil {
			return nil, resp.Header, fmt.Errorf("%s decode SSE frame: %w", method, err)
		}
	} else {
		if err := json.Unmarshal(body, &rpcResp); err != nil {
			return nil, resp.Header, fmt.Errorf("%s decode JSON: %w", method, err)
		}
	}
	if rpcResp.Error != nil {
		return nil, resp.Header, fmt.Errorf("%s: rpc error %d: %s", method, rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, resp.Header, nil
}

// firstSSEDataFrame extracts the first `data: ...` payload from an
// SSE-encoded body. Playwright MCP's streamable HTTP responses for a
// single-shot call contain exactly one event, so we don't need a
// full event-stream parser.
func firstSSEDataFrame(body []byte) ([]byte, error) {
	var out bytes.Buffer
	lines := bytes.Split(body, []byte("\n"))
	sawData := false
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			if sawData {
				return out.Bytes(), nil
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimPrefix(line, []byte("data:"))
			payload = bytes.TrimPrefix(payload, []byte(" "))
			out.Write(payload)
			sawData = true
		}
	}
	if !sawData {
		return nil, fmt.Errorf("no data frame in SSE body")
	}
	return out.Bytes(), nil
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
