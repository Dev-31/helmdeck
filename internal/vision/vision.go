// Package vision provides the shared core for helmdeck's vision-mode
// action loop (T407, T408, ADR 027). It is consumed by both the
// /api/v1/sessions/{id}/vision/act REST endpoint and by the reference
// vision packs (vision.click_anywhere, vision.extract_visible_text,
// vision.fill_form_by_label).
//
// Why a separate package: api/vision.go and packs/builtin/vision_*.go
// both need the same screenshot → multimodal model call → parse →
// dispatch pipeline. Putting it under internal/api would force packs
// to import api (cycle); putting it under internal/packs would force
// the API to import packs (also a cycle today). A leaf package on
// both gateway and session is the cleanest decoupling.
package vision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tosin2013/helmdeck/internal/gateway"
	"github.com/tosin2013/helmdeck/internal/session"
)

// Dispatcher is the gateway surface this package depends on. Both
// *gateway.Registry and *gateway.Chain satisfy it; tests can stub
// the single method.
type Dispatcher interface {
	Dispatch(ctx context.Context, req gateway.ChatRequest) (gateway.ChatResponse, error)
}

// Action is the structured response shape the vision model is
// instructed to emit. Exported so the reference packs and tests can
// build expected fixtures.
type Action struct {
	Action string `json:"action"`
	X      int    `json:"x,omitempty"`
	Y      int    `json:"y,omitempty"`
	Text   string `json:"text,omitempty"`
	Keys   string `json:"keys,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// SystemPrompt is the system message every vision call sends. The
// strict-JSON instruction is centralised so both the REST endpoint
// and the reference packs share the same prompt — drift between the
// two would mean the parser quietly disagrees with what the model
// was told to emit.
const SystemPrompt = `You control a Linux desktop. You will see a screenshot of the current screen and a user goal. Decide the SINGLE next action to advance toward the goal.

Respond with ONE JSON object and nothing else. Do not wrap it in markdown. The schema:

{
  "action": "click" | "type" | "key" | "none" | "done",
  "x":      <integer pixel x for click, required if action is click>,
  "y":      <integer pixel y for click, required if action is click>,
  "text":   <string to type, required if action is type>,
  "keys":   <xdotool key spec like "Return" or "ctrl+a", required if action is key>,
  "reason": <one-sentence explanation>
}

Use "done" when the goal is achieved. Use "none" if no action is appropriate this turn.`

// CaptureScreenshot runs scrot inside the session container and
// returns the PNG bytes. Same temp-file dance as the desktop REST
// endpoint so it works against scrot 1.0+.
func CaptureScreenshot(ctx context.Context, ex session.Executor, sessionID string) ([]byte, error) {
	tmp := "/tmp/helmdeck-vision-shot.png"
	res, err := ex.Exec(ctx, sessionID, session.ExecRequest{
		Cmd: []string{"sh", "-c", "scrot -o " + tmp + " >/dev/null && cat " + tmp + " && rm -f " + tmp},
		Env: []string{"DISPLAY=:99"},
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("scrot exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	if len(res.Stdout) == 0 {
		return nil, errors.New("scrot produced no output")
	}
	return res.Stdout, nil
}

// AskModel sends one screenshot + goal to the dispatcher and returns
// the model's raw text response. Callers run ParseAction on the
// result. The model + max_tokens come from the caller; the system
// prompt and message shape are fixed.
func AskModel(ctx context.Context, d Dispatcher, model, goal string, png []byte, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = 512
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	req := gateway.ChatRequest{
		Model:     model,
		MaxTokens: &maxTokens,
		Messages: []gateway.Message{
			{Role: "system", Content: gateway.TextContent(SystemPrompt)},
			{Role: "user", Content: gateway.MultipartContent(
				gateway.TextPart("Goal: " + goal),
				gateway.ImageURLPartFromURL(dataURL),
			)},
		},
	}
	resp, err := d.Dispatch(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("model returned no choices")
	}
	return resp.Choices[0].Message.Content.Text(), nil
}

// ParseAction decodes the model's response into an Action. Strict
// json.Unmarshal is tried first; on failure we extract the first
// balanced {...} block from the response and try again. Tolerates
// frontier models that wrap JSON in markdown code fences and weak
// models that emit a sentence of prose around it.
func ParseAction(raw string) (Action, error) {
	raw = strings.TrimSpace(raw)
	var a Action
	if err := json.Unmarshal([]byte(raw), &a); err == nil {
		if a.Action == "" {
			return a, errors.New("action field is empty")
		}
		return a, nil
	}
	if obj := extractFirstJSONObject(raw); obj != "" {
		if err := json.Unmarshal([]byte(obj), &a); err == nil {
			if a.Action == "" {
				return a, errors.New("action field is empty")
			}
			return a, nil
		}
	}
	return a, errors.New("no parseable JSON object found")
}

// DispatchAction maps an Action onto a session.Executor invocation.
// Returns (executed, err) where executed indicates whether any side
// effect was attempted — "none" and "done" are valid no-ops.
func DispatchAction(ctx context.Context, ex session.Executor, sessionID string, a Action) (bool, error) {
	switch strings.ToLower(a.Action) {
	case "none", "done", "":
		return false, nil
	case "click":
		cmd := []string{"sh", "-c", fmt.Sprintf("xdotool mousemove %d %d click 1", a.X, a.Y)}
		return runCmd(ctx, ex, sessionID, cmd)
	case "type":
		if a.Text == "" {
			return false, errors.New("type action missing text field")
		}
		return runCmd(ctx, ex, sessionID, []string{"xdotool", "type", "--", a.Text})
	case "key":
		if a.Keys == "" {
			return false, errors.New("key action missing keys field")
		}
		return runCmd(ctx, ex, sessionID, []string{"xdotool", "key", "--", a.Keys})
	default:
		return false, fmt.Errorf("unknown action %q", a.Action)
	}
}

// Step performs one full vision iteration: capture screenshot, ask
// model, parse, dispatch. Returns the parsed action plus the raw
// model response (useful for audit logging) plus a flag indicating
// whether a side effect was attempted. Used by the reference packs
// to drive their own loops.
type StepResult struct {
	Action        Action
	Executed      bool
	ModelResponse string
	Screenshot    []byte
}

func Step(ctx context.Context, d Dispatcher, ex session.Executor, sessionID, model, goal string, maxTokens int) (StepResult, error) {
	png, err := CaptureScreenshot(ctx, ex, sessionID)
	if err != nil {
		return StepResult{}, fmt.Errorf("screenshot: %w", err)
	}
	raw, err := AskModel(ctx, d, model, goal, png, maxTokens)
	if err != nil {
		return StepResult{}, fmt.Errorf("model call: %w", err)
	}
	action, err := ParseAction(raw)
	if err != nil {
		return StepResult{ModelResponse: raw, Screenshot: png},
			fmt.Errorf("parse action: %w", err)
	}
	executed, derr := DispatchAction(ctx, ex, sessionID, action)
	if derr != nil {
		return StepResult{Action: action, ModelResponse: raw, Screenshot: png},
			fmt.Errorf("dispatch action: %w", derr)
	}
	return StepResult{Action: action, Executed: executed, ModelResponse: raw, Screenshot: png}, nil
}

func runCmd(ctx context.Context, ex session.Executor, sessionID string, cmd []string) (bool, error) {
	res, err := ex.Exec(ctx, sessionID, session.ExecRequest{
		Cmd: cmd,
		Env: []string{"DISPLAY=:99"},
	})
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return true, nil
}

// extractFirstJSONObject scans for the first balanced { ... } block.
// Doesn't handle quoted braces inside strings perfectly — good
// enough for the action JSON shape which has no string values that
// contain braces.
func extractFirstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inString {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
