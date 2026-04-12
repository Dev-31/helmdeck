package pwmcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubPlaywrightMCP is a minimal Playwright MCP stand-in for unit
// tests. It speaks the JSON-RPC subset the client exercises:
// initialize returns an empty result and sets Mcp-Session-Id; every
// other method dispatches through the `handlers` map keyed by
// JSON-RPC method name. Notifications are accepted but drop the body.
func stubPlaywrightMCP(t *testing.T, handlers map[string]func(params json.RawMessage) (result any, rpcErrCode int, rpcErrMsg string)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int64           `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json: "+err.Error(), 400)
			return
		}

		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(202)
			return
		}

		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-1234")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"protocolVersion": "2025-03-26"},
			})
			return
		}

		if got := r.Header.Get("Mcp-Session-Id"); got != "sess-1234" {
			http.Error(w, "missing Mcp-Session-Id, got: "+got, 401)
			return
		}

		h, ok := handlers[req.Method]
		if !ok {
			http.Error(w, "no handler for "+req.Method, 404)
			return
		}
		res, code, msg := h(req.Params)
		w.Header().Set("Content-Type", "application/json")
		if code != 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": code, "message": msg},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  res,
		})
	})
	return httptest.NewServer(mux)
}

func TestClient_Initialize_PersistsSessionID(t *testing.T) {
	srv := stubPlaywrightMCP(t, map[string]func(json.RawMessage) (any, int, string){
		"tools/call": func(params json.RawMessage) (any, int, string) {
			// Just echoes the arguments back as a text block so the
			// test can assert Mcp-Session-Id made it through — the
			// stub returns 401 if the header is missing.
			return map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "ok"},
				},
			}, 0, ""
		},
	})
	defer srv.Close()

	c := New(srv.URL+"/mcp", nil)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := c.ToolsCall(context.Background(), "browser_snapshot", nil)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("tool text = %q, want %q", res.Text, "ok")
	}
	if res.IsError {
		t.Errorf("is_error = true, want false")
	}
}

func TestClient_ToolsCall_SurfacesRPCError(t *testing.T) {
	srv := stubPlaywrightMCP(t, map[string]func(json.RawMessage) (any, int, string){
		"tools/call": func(params json.RawMessage) (any, int, string) {
			return nil, -32602, "element not found"
		},
	})
	defer srv.Close()

	c := New(srv.URL+"/mcp", nil)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	_, err := c.ToolsCall(context.Background(), "browser_click", map[string]any{"ref": "e1"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "element not found") {
		t.Errorf("error = %v, want contains 'element not found'", err)
	}
}

func TestClient_ToolsCall_IsErrorFlag(t *testing.T) {
	// Playwright MCP marks tool-level failures (e.g. "timeout waiting
	// for element") with isError=true in the result body rather than
	// a JSON-RPC error — the call itself succeeded, but the action
	// didn't. The client must preserve that flag so the pack can
	// record the failure in its step trace instead of aborting the
	// whole run on the first missed click.
	srv := stubPlaywrightMCP(t, map[string]func(json.RawMessage) (any, int, string){
		"tools/call": func(params json.RawMessage) (any, int, string) {
			return map[string]any{
				"isError": true,
				"content": []map[string]any{
					{"type": "text", "text": "timeout 5000ms waiting for element"},
				},
			}, 0, ""
		},
	})
	defer srv.Close()

	c := New(srv.URL+"/mcp", nil)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := c.ToolsCall(context.Background(), "browser_click", map[string]any{"ref": "eX"})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if !res.IsError {
		t.Errorf("is_error = false, want true")
	}
	if !strings.Contains(res.Text, "timeout") {
		t.Errorf("text = %q, want contains 'timeout'", res.Text)
	}
}

func TestClient_ToolsCall_SSEResponseBody(t *testing.T) {
	// Streamable HTTP can return text/event-stream for tool calls,
	// not just application/json. Make sure the SSE parser pulls the
	// data frame out correctly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-sse")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2025-03-26"},
			})
			return
		}
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "snapshot-via-sse"},
				},
			},
		})
		_, _ = w.Write([]byte("event: message\n"))
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(payload)
		_, _ = w.Write([]byte("\n\n"))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := c.ToolsCall(context.Background(), "browser_snapshot", nil)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.Text != "snapshot-via-sse" {
		t.Errorf("text = %q, want snapshot-via-sse", res.Text)
	}
}

func TestClient_ToolsCall_Upstream5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{},
			})
			return
		}
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(202)
			return
		}
		http.Error(w, "upstream exploded", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	_, err := c.ToolsCall(context.Background(), "browser_snapshot", nil)
	if err == nil {
		t.Fatalf("expected upstream error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error = %v, want contains 502", err)
	}
}
