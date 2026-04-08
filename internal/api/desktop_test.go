package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/session"
)

// fakeExecutor records every Exec call and replays a scripted reply.
// Tests can inspect captured calls to assert on the exact xdotool /
// scrot argv the desktop endpoints emit.
type fakeExecutor struct {
	calls []capturedExec
	// reply is the canned response. If err is set, ExecResult is
	// ignored. resultByCmd lets a test return different replies for
	// different argv0 values (used by the windows listing test).
	reply       session.ExecResult
	err         error
	resultByCmd map[string]session.ExecResult
}

type capturedExec struct {
	SessionID string
	Cmd       []string
	Env       []string
}

func (f *fakeExecutor) Exec(ctx context.Context, id string, req session.ExecRequest) (session.ExecResult, error) {
	f.calls = append(f.calls, capturedExec{SessionID: id, Cmd: req.Cmd, Env: req.Env})
	if f.err != nil {
		return session.ExecResult{}, f.err
	}
	if f.resultByCmd != nil {
		// Match against the joined script body for sh -c invocations,
		// otherwise the argv0 of a non-shell call.
		key := req.Cmd[0]
		if len(req.Cmd) >= 3 && req.Cmd[0] == "sh" && req.Cmd[1] == "-c" {
			key = req.Cmd[2]
		}
		for k, v := range f.resultByCmd {
			if strings.Contains(key, k) {
				return v, nil
			}
		}
	}
	return f.reply, nil
}

func newDesktopRouter(t *testing.T, ex *fakeExecutor) http.Handler {
	t.Helper()
	return NewRouter(Deps{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:  "test",
		Executor: ex,
	})
}

func doDesktop(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestDesktopScreenshot(t *testing.T) {
	pngHead := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00}
	ex := &fakeExecutor{reply: session.ExecResult{Stdout: pngHead}}
	h := newDesktopRouter(t, ex)
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/screenshot",
		`{"session_id":"sess-1"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Type") != "image/png" {
		t.Errorf("wrong content-type: %s", rr.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(rr.Body.String(), "\x89PNG") {
		t.Errorf("body is not a PNG: %q", rr.Body.String()[:8])
	}
	if len(ex.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(ex.calls))
	}
	if ex.calls[0].SessionID != "sess-1" {
		t.Errorf("session id not propagated: %s", ex.calls[0].SessionID)
	}
	if !strings.Contains(ex.calls[0].Env[0], "DISPLAY=:99") {
		t.Errorf("DISPLAY env not set: %v", ex.calls[0].Env)
	}
}

func TestDesktopClick(t *testing.T) {
	cases := []struct{ button, want string }{
		{"", "1"},
		{"left", "1"},
		{"middle", "2"},
		{"right", "3"},
	}
	for _, tc := range cases {
		t.Run(tc.button, func(t *testing.T) {
			ex := &fakeExecutor{}
			h := newDesktopRouter(t, ex)
			body := `{"session_id":"sess-1","x":42,"y":99`
			if tc.button != "" {
				body += `,"button":"` + tc.button + `"`
			}
			body += `}`
			rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/click", body)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			joined := strings.Join(ex.calls[0].Cmd, " ")
			if !strings.Contains(joined, "mousemove 42 99 click "+tc.want) {
				t.Errorf("wrong cmd: %s", joined)
			}
		})
	}
}

func TestDesktopClickInvalidButton(t *testing.T) {
	h := newDesktopRouter(t, &fakeExecutor{})
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/click",
		`{"session_id":"s","x":1,"y":2,"button":"laser"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestDesktopType(t *testing.T) {
	ex := &fakeExecutor{}
	h := newDesktopRouter(t, ex)
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/type",
		`{"session_id":"s","text":"hello world","delay_ms":50}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	cmd := ex.calls[0].Cmd
	// argv: xdotool type --delay 50 -- "hello world"
	if cmd[0] != "xdotool" || cmd[1] != "type" || cmd[len(cmd)-1] != "hello world" {
		t.Errorf("bad argv: %v", cmd)
	}
}

func TestDesktopTypeMissingText(t *testing.T) {
	h := newDesktopRouter(t, &fakeExecutor{})
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/type", `{"session_id":"s","text":""}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestDesktopKey(t *testing.T) {
	ex := &fakeExecutor{}
	h := newDesktopRouter(t, ex)
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/key",
		`{"session_id":"s","keys":"ctrl+a"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if ex.calls[0].Cmd[len(ex.calls[0].Cmd)-1] != "ctrl+a" {
		t.Errorf("keys not propagated: %v", ex.calls[0].Cmd)
	}
}

func TestDesktopLaunch(t *testing.T) {
	ex := &fakeExecutor{}
	h := newDesktopRouter(t, ex)
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/launch",
		`{"session_id":"s","command":"xterm","args":["-e","echo hi"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	script := strings.Join(ex.calls[0].Cmd, " ")
	if !strings.Contains(script, "nohup setsid 'xterm' '-e' 'echo hi'") {
		t.Errorf("launch script wrong: %s", script)
	}
}

func TestDesktopWindowsListing(t *testing.T) {
	ex := &fakeExecutor{
		resultByCmd: map[string]session.ExecResult{
			"xdotool search": {Stdout: []byte("12345\t100\tFirefox\n67890\t200\tTerminal\n")},
		},
	}
	h := newDesktopRouter(t, ex)
	rr := doDesktop(t, h, http.MethodGet, "/api/v1/desktop/windows?session_id=s", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Windows []DesktopWindow `json:"windows"`
		Count   int             `json:"count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 2 {
		t.Fatalf("expected 2 windows, got %d: %s", resp.Count, rr.Body.String())
	}
	if resp.Windows[0].ID != "12345" || resp.Windows[0].Name != "Firefox" || resp.Windows[0].PID != 100 {
		t.Errorf("first window parsed wrong: %+v", resp.Windows[0])
	}
}

func TestDesktopFocus(t *testing.T) {
	ex := &fakeExecutor{}
	h := newDesktopRouter(t, ex)
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/focus",
		`{"session_id":"s","window_id":"12345"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	cmd := ex.calls[0].Cmd
	if cmd[len(cmd)-1] != "12345" || cmd[1] != "windowactivate" {
		t.Errorf("bad argv: %v", cmd)
	}
}

func TestDesktopFocusRejectsInjection(t *testing.T) {
	h := newDesktopRouter(t, &fakeExecutor{})
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/focus",
		`{"session_id":"s","window_id":"12345; rm -rf /"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-numeric window id, got %d", rr.Code)
	}
}

func TestDesktopMissingSessionID(t *testing.T) {
	h := newDesktopRouter(t, &fakeExecutor{})
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/click",
		`{"x":1,"y":2}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestDesktopNoExecutorReturns503(t *testing.T) {
	h := NewRouter(Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
	})
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/click",
		`{"session_id":"s","x":1,"y":2}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
}

func TestDesktopExecFailureMaps502(t *testing.T) {
	ex := &fakeExecutor{reply: session.ExecResult{ExitCode: 1, Stderr: []byte("Can't open display")}}
	h := newDesktopRouter(t, ex)
	rr := doDesktop(t, h, http.MethodPost, "/api/v1/desktop/click",
		`{"session_id":"s","x":1,"y":2}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "command_failed") {
		t.Errorf("expected command_failed code: %s", rr.Body.String())
	}
}
