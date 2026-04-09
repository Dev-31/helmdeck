// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package api

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// readSSEFrame reads one complete `event: …\ndata: …\n\n` block
// from r. Returns (event, data, error).
func readSSEFrame(t *testing.T, br *bufio.Reader) (event, data string) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("sse read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestMCPSSE_HandshakeReturnsEndpoint(t *testing.T) {
	srv := httptest.NewServer(newMCPServerRouter(t))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/mcp/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get sse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	event, data := readSSEFrame(t, br)
	if event != "endpoint" {
		t.Errorf("first event = %q want endpoint", event)
	}
	matched, _ := regexp.MatchString(`^/api/v1/mcp/sse/message\?sessionId=[0-9a-f]+$`, data)
	if !matched {
		t.Errorf("endpoint data = %q", data)
	}
}

func TestMCPSSE_ListAndCallRoundTrip(t *testing.T) {
	srv := httptest.NewServer(newMCPServerRouter(t))
	defer srv.Close()

	// Open SSE stream.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/mcp/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get sse: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	// First frame: endpoint.
	_, endpoint := readSSEFrame(t, br)
	if endpoint == "" {
		t.Fatal("no endpoint frame")
	}

	postFrame := func(body string) {
		r, err := http.Post(srv.URL+endpoint, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(r.Body)
			t.Fatalf("post status = %d: %s", r.StatusCode, b)
		}
	}

	// initialize first per MCP handshake.
	postFrame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	_, init := readSSEFrame(t, br)
	if !strings.Contains(init, `"protocolVersion"`) {
		t.Errorf("init resp = %s", init)
	}

	// tools/list — echo pack should appear.
	postFrame(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	_, list := readSSEFrame(t, br)
	if !strings.Contains(list, `"echo"`) {
		t.Errorf("list resp = %s", list)
	}

	// tools/call — echo back hi.
	postFrame(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hi"}}}`)
	_, call := readSSEFrame(t, br)
	if !strings.Contains(call, `"isError":false`) {
		t.Errorf("call resp missing isError:false: %s", call)
	}
	if !strings.Contains(call, `\"echo\":\"hi\"`) {
		t.Errorf("call resp missing echoed text: %s", call)
	}
}

func TestMCPSSE_PostUnknownSessionIs404(t *testing.T) {
	srv := httptest.NewServer(newMCPServerRouter(t))
	defer srv.Close()

	r, err := http.Post(srv.URL+"/api/v1/mcp/sse/message?sessionId=nope", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", r.StatusCode)
	}
}

func TestMCPSSE_PostMissingSessionIs400(t *testing.T) {
	srv := httptest.NewServer(newMCPServerRouter(t))
	defer srv.Close()

	r, err := http.Post(srv.URL+"/api/v1/mcp/sse/message", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", r.StatusCode)
	}
}

func TestMCPSSE_UnavailableWhenNil(t *testing.T) {
	h := NewRouter(Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
	})
	for _, path := range []string{"/api/v1/mcp/sse", "/api/v1/mcp/sse/message"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d", path, rr.Code)
		}
	}
}

// silence "imported and not used" if time becomes optional later.
var _ = time.Second
