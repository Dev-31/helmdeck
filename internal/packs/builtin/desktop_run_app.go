package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
)

// DesktopRunAppAndScreenshot (T402, ADR 027) is the reference desktop
// pack: spawn an arbitrary application in a desktop-mode session,
// wait for it to render, capture the screen, and return the PNG as
// an artifact. Together with screenshot_url it forms the smallest
// possible "see-something" surface for agents that need GUI output.
//
// The pack acquires its own session (NeedsSession=true) and asks the
// runtime for desktop mode by setting HELMDECK_MODE=desktop on the
// session env. The sidecar entrypoint reads that env var and starts
// Xvfb + XFCE4 instead of headless Chromium.
//
// Input shape:
//
//	{
//	  "command": "xterm",            // required
//	  "args":    ["-e", "echo hi"],  // optional
//	  "wait_ms": 1500                 // optional, default 1500
//	}
//
// Output shape:
//
//	{
//	  "artifact_key": "desktop.run_app_and_screenshot/<rand>-screen.png",
//	  "size":         <int>,
//	  "command":      "xterm"
//	}
func DesktopRunAppAndScreenshot() *packs.Pack {
	return &packs.Pack{
		Name:        "desktop.run_app_and_screenshot",
		Version:     "v1",
		Description: "Launch an application inside a desktop-mode session and return a screenshot of the result.",
		NeedsSession: true,
		SessionSpec: session.Spec{
			// Desktop mode is the only difference from a normal browser
			// session. The runtime defaults supply image/limits/timeout.
			Env: map[string]string{"HELMDECK_MODE": "desktop"},
		},
		InputSchema: packs.BasicSchema{
			Required: []string{"command"},
			Properties: map[string]string{
				"command": "string",
				"args":    "array",
				"wait_ms": "number",
			},
		},
		OutputSchema: packs.BasicSchema{
			Required: []string{"artifact_key", "size", "command"},
			Properties: map[string]string{
				"artifact_key": "string",
				"size":         "number",
				"command":      "string",
			},
		},
		Handler: desktopRunAppHandler,
	}
}

type desktopRunAppInput struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	WaitMS  int      `json:"wait_ms"`
}

func desktopRunAppHandler(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
	var in desktopRunAppInput
	if err := json.Unmarshal(ec.Input, &in); err != nil {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
	}
	if strings.TrimSpace(in.Command) == "" {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "command must not be empty"}
	}
	if ec.Exec == nil {
		return nil, &packs.PackError{Code: packs.CodeSessionUnavailable, Message: "engine has no session executor"}
	}
	wait := in.WaitMS
	if wait <= 0 {
		wait = 1500
	}
	if wait > 60000 {
		// Cap to one minute — packs should not block the engine for
		// arbitrary intervals; longer waits are an antipattern.
		wait = 60000
	}

	// Launch the app detached so it survives the Exec rpc. nohup +
	// setsid + & is the same shape the desktop REST endpoint uses.
	// Single-quote escaping keeps user-supplied args shell-safe.
	quoted := []string{shellQuote(in.Command)}
	for _, a := range in.Args {
		quoted = append(quoted, shellQuote(a))
	}
	launch := "nohup setsid " + strings.Join(quoted, " ") + " >/dev/null 2>&1 &"
	if _, err := ec.Exec(ctx, session.ExecRequest{
		Cmd: []string{"sh", "-c", launch},
		Env: []string{"DISPLAY=:99"},
	}); err != nil {
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: fmt.Sprintf("launch %s: %v", in.Command, err)}
	}

	// Wait for the app to render. select on ctx so a cancelled
	// pack call returns promptly instead of sleeping the full wait.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(time.Duration(wait) * time.Millisecond):
	}

	// Capture the desktop. Use the same temp-file dance as the
	// desktop REST endpoint so we don't depend on a particular
	// scrot version's stdout support.
	tmp := "/tmp/helmdeck-pack-shot.png"
	res, err := ec.Exec(ctx, session.ExecRequest{
		Cmd: []string{"sh", "-c", "scrot -o " + tmp + " >/dev/null && cat " + tmp + " && rm -f " + tmp},
		Env: []string{"DISPLAY=:99"},
	})
	if err != nil {
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: fmt.Sprintf("scrot: %v", err)}
	}
	if res.ExitCode != 0 {
		stderr := string(res.Stderr)
		if len(stderr) > 512 {
			stderr = stderr[:512] + "...(truncated)"
		}
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
			Message: fmt.Sprintf("scrot exit %d: %s", res.ExitCode, stderr)}
	}
	if len(res.Stdout) == 0 {
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: "scrot produced empty output"}
	}

	art, err := ec.Artifacts.Put(ctx, ec.Pack.Name, "screen.png", res.Stdout, "image/png")
	if err != nil {
		return nil, &packs.PackError{Code: packs.CodeArtifactFailed, Message: err.Error(), Cause: err}
	}
	return json.Marshal(map[string]any{
		"artifact_key": art.Key,
		"size":         art.Size,
		"command":      in.Command,
	})
}

// shellQuote wraps an arg in single quotes for safe shell inclusion.
// Same implementation as internal/api/desktop.go's shellQuote, kept
// duplicated to avoid an api ↔ packs/builtin import cycle for one
// six-line function.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
