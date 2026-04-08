// Package api — desktop actions REST surface (T401, ADR 027).
//
// The desktop endpoints expose xdotool/scrot driven by the existing
// session.Executor against any session container running in desktop
// mode (Xvfb on DISPLAY=:99 + XFCE4 + chromium, started by
// deploy/docker/sidecar-entrypoint.sh when SIDECAR_MODE=desktop).
//
// Every endpoint follows the same shape as the browser CDP endpoints:
// JSON request body, session_id field, executor lookup, typed JSON
// response or PNG bytes for screenshot. JWT enforcement and audit
// logging come for free from the /api/v1/* prefix.
//
// Command injection: xdotool's `type` and `key` subcommands take the
// payload as a single arg, and we always pass argv via the session
// Executor's Cmd []string (no shell expansion) so user input cannot
// escape into a shell context. The launch endpoint runs an arbitrary
// command inside the session container — no different from the
// existing /api/v1/browser/execute surface in terms of trust model;
// the sandbox boundary is the session container, not the API.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tosin2013/helmdeck/internal/session"
)

// desktopDisplay is the X server number the desktop-mode sidecar
// entrypoint starts Xvfb on. Every xdotool/scrot invocation needs
// DISPLAY=:99 in its env or it will fail with "Can't open display".
const desktopDisplay = ":99"

// Maximum response sizes — guards against runaway scrot output or
// xdotool windows scans on a desktop with thousands of windows.
const (
	maxScreenshotBytes = 32 << 20 // 32 MiB
	maxWindowsListed   = 1024
)

type desktopClickRequest struct {
	SessionID string `json:"session_id"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Button    string `json:"button,omitempty"` // left|right|middle (default left)
}

type desktopTypeRequest struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
	DelayMS   int    `json:"delay_ms,omitempty"` // per-keystroke delay
}

type desktopKeyRequest struct {
	SessionID string `json:"session_id"`
	Keys      string `json:"keys"` // xdotool key spec, e.g. "ctrl+a", "Return"
}

type desktopLaunchRequest struct {
	SessionID string   `json:"session_id"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
}

type desktopFocusRequest struct {
	SessionID string `json:"session_id"`
	WindowID  string `json:"window_id"`
}

type desktopScreenshotRequest struct {
	SessionID string `json:"session_id"`
}

// DesktopWindow is one entry in the windows listing. ID is the X11
// window id (decimal string from xdotool), Name is the window title,
// PID is the owning process id (best-effort; some windows lack one).
type DesktopWindow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	PID  int    `json:"pid,omitempty"`
}

func registerDesktopRoutes(mux *http.ServeMux, deps Deps) {
	if deps.Executor == nil {
		mux.HandleFunc("/api/v1/desktop/", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "executor_unavailable",
				"desktop actions require a session.Executor backend")
		})
		return
	}
	ex := deps.Executor

	// run is a small wrapper that injects DISPLAY and maps Executor
	// errors / non-zero exit codes onto the desktop endpoints' typed
	// error vocabulary. Returns the result so callers can inspect
	// stdout when they need to (e.g. windows listing, screenshot).
	run := func(w http.ResponseWriter, r *http.Request, sessionID string, cmd []string) (session.ExecResult, bool) {
		if sessionID == "" {
			writeError(w, http.StatusBadRequest, "missing_session_id", "session_id is required")
			return session.ExecResult{}, false
		}
		res, err := ex.Exec(r.Context(), sessionID, session.ExecRequest{
			Cmd: cmd,
			Env: []string{"DISPLAY=" + desktopDisplay},
		})
		if err != nil {
			if errors.Is(err, session.ErrSessionNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return session.ExecResult{}, false
			}
			writeError(w, http.StatusBadGateway, "exec_failed", err.Error())
			return session.ExecResult{}, false
		}
		if res.ExitCode != 0 {
			writeError(w, http.StatusBadGateway, "command_failed",
				fmt.Sprintf("exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr))))
			return session.ExecResult{}, false
		}
		return res, true
	}

	mux.HandleFunc("POST /api/v1/desktop/screenshot", func(w http.ResponseWriter, r *http.Request) {
		var req desktopScreenshotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		// scrot writes to a temp file then we cat it back. Using
		// `scrot -` (stdout) is only supported in scrot ≥1.2 and the
		// sidecar's apt repo may ship 1.0; the temp-file dance is
		// portable. -o overwrites silently.
		tmp := "/tmp/helmdeck-shot.png"
		res, ok := run(w, r, req.SessionID, []string{
			"sh", "-c",
			"scrot -o " + tmp + " >/dev/null && cat " + tmp + " && rm -f " + tmp,
		})
		if !ok {
			return
		}
		if len(res.Stdout) == 0 {
			writeError(w, http.StatusBadGateway, "command_failed", "scrot produced no output")
			return
		}
		if len(res.Stdout) > maxScreenshotBytes {
			writeError(w, http.StatusInternalServerError, "screenshot_too_large",
				fmt.Sprintf("scrot returned %d bytes (max %d)", len(res.Stdout), maxScreenshotBytes))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.Stdout)
	})

	mux.HandleFunc("POST /api/v1/desktop/click", func(w http.ResponseWriter, r *http.Request) {
		var req desktopClickRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		button := "1"
		switch strings.ToLower(req.Button) {
		case "", "left":
			button = "1"
		case "middle":
			button = "2"
		case "right":
			button = "3"
		default:
			writeError(w, http.StatusBadRequest, "invalid_button",
				"button must be left, middle, or right")
			return
		}
		// xdotool mousemove + click in one invocation: pass the two
		// commands joined with `--`-style separators isn't supported,
		// so chain via sh -c with explicit numeric args (the integers
		// can't carry shell injection).
		cmd := []string{
			"sh", "-c",
			fmt.Sprintf("xdotool mousemove %d %d click %s",
				req.X, req.Y, button),
		}
		if _, ok := run(w, r, req.SessionID, cmd); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "x": req.X, "y": req.Y, "button": button})
	})

	mux.HandleFunc("POST /api/v1/desktop/type", func(w http.ResponseWriter, r *http.Request) {
		var req desktopTypeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if req.Text == "" {
			writeError(w, http.StatusBadRequest, "missing_text", "text is required")
			return
		}
		// xdotool type takes the literal string as a single arg —
		// passing via Cmd []string keeps it out of any shell context.
		cmd := []string{"xdotool", "type"}
		if req.DelayMS > 0 {
			cmd = append(cmd, "--delay", strconv.Itoa(req.DelayMS))
		}
		cmd = append(cmd, "--", req.Text)
		if _, ok := run(w, r, req.SessionID, cmd); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "length": len(req.Text)})
	})

	mux.HandleFunc("POST /api/v1/desktop/key", func(w http.ResponseWriter, r *http.Request) {
		var req desktopKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if req.Keys == "" {
			writeError(w, http.StatusBadRequest, "missing_keys", "keys is required")
			return
		}
		if _, ok := run(w, r, req.SessionID, []string{"xdotool", "key", "--", req.Keys}); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": req.Keys})
	})

	mux.HandleFunc("POST /api/v1/desktop/launch", func(w http.ResponseWriter, r *http.Request) {
		var req desktopLaunchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if req.Command == "" {
			writeError(w, http.StatusBadRequest, "missing_command", "command is required")
			return
		}
		// nohup + setsid + & so the launched process outlives this
		// exec rpc and detaches from xdotool's session — otherwise
		// the application would die when our Exec returns.
		quoted := []string{shellQuote(req.Command)}
		for _, a := range req.Args {
			quoted = append(quoted, shellQuote(a))
		}
		cmd := []string{
			"sh", "-c",
			"nohup setsid " + strings.Join(quoted, " ") + " >/dev/null 2>&1 &",
		}
		if _, ok := run(w, r, req.SessionID, cmd); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "command": req.Command})
	})

	mux.HandleFunc("GET /api/v1/desktop/windows", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("session_id")
		// xdotool search "" returns every window id, one per line.
		// We then resolve names + pids in a single shell loop so we
		// don't pay an Exec rpc per window.
		script := `
ids=$(xdotool search --onlyvisible "" 2>/dev/null || true)
for id in $ids; do
  name=$(xdotool getwindowname "$id" 2>/dev/null || true)
  pid=$(xdotool getwindowpid "$id" 2>/dev/null || echo 0)
  printf '%s\t%s\t%s\n' "$id" "$pid" "$name"
done
`
		res, ok := run(w, r, sessionID, []string{"sh", "-c", script})
		if !ok {
			return
		}
		var out []DesktopWindow
		for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) < 3 {
				continue
			}
			pid, _ := strconv.Atoi(parts[1])
			out = append(out, DesktopWindow{ID: parts[0], PID: pid, Name: parts[2]})
			if len(out) >= maxWindowsListed {
				break
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"windows": out, "count": len(out)})
	})

	mux.HandleFunc("POST /api/v1/desktop/focus", func(w http.ResponseWriter, r *http.Request) {
		var req desktopFocusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if req.WindowID == "" {
			writeError(w, http.StatusBadRequest, "missing_window_id", "window_id is required")
			return
		}
		// windowactivate brings the window to the front and sets it
		// as the focused window — the next type/key call will land
		// in it. Reject non-numeric window ids before shelling out.
		if _, err := strconv.ParseUint(req.WindowID, 10, 64); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_window_id", "window_id must be a numeric X11 window id")
			return
		}
		if _, ok := run(w, r, req.SessionID, []string{"xdotool", "windowactivate", "--sync", req.WindowID}); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "window_id": req.WindowID})
	})
}

// shellQuote wraps an arg in single quotes for safe inclusion in a
// shell command line. The only character we have to escape is the
// single quote itself; replace ' with '\'' (close, escaped, reopen).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
