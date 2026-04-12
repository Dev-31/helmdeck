package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
	"github.com/tosin2013/helmdeck/internal/vision"
)

// Reference vision packs (T408, ADR 027). Each one wraps the
// vision.Step pipeline (screenshot → multimodal model call → parse →
// dispatch) under a typed pack interface so weak models can call them
// without having to drive the action loop themselves.
//
// All three packs require:
//   - a session.Executor (for screenshot + xdotool dispatch)
//   - a vision.Dispatcher (the AI gateway, threaded in via constructor
//     injection so the engine doesn't need a new option)
//   - a session created in HELMDECK_MODE=desktop (the pack requests
//     this via SessionSpec.Env, mirroring desktop.run_app_and_screenshot)
//
// Per-pack TTL is left at the platform default — vision artifacts
// are usually intermediate state, not user-facing deliverables.

const (
	defaultVisionMaxSteps = 6
	defaultVisionMaxTokens = 512
)

// VisionClickAnywhere (T408) — pack interface for "find and click on
// the thing matching this description". Iterates the vision step
// loop until the model returns done, click+done, or max_steps is
// hit. Returns the final action plus a step trace.
//
// Input shape:
//
//	{
//	  "goal":      "click the Sign In button",   // required
//	  "model":     "openai/gpt-4o",               // required, provider/model
//	  "max_steps": 4                              // optional, default 6
//	}
//
// Output shape:
//
//	{
//	  "completed": true,
//	  "steps":     2,
//	  "final_action": {"action": "done", "reason": "..."}
//	}
func VisionClickAnywhere(d vision.Dispatcher) *packs.Pack {
	return &packs.Pack{
		Name:        "vision.click_anywhere",
		Version:     "v1",
		Description: "Find and click an on-screen target by description, using a vision model.",
		NeedsSession: true,
		SessionSpec: session.Spec{Env: map[string]string{"HELMDECK_MODE": "desktop"}},
		InputSchema: packs.BasicSchema{
			Required: []string{"goal", "model"},
			Properties: map[string]string{
				"goal":      "string",
				"model":     "string",
				"max_steps": "number",
			},
		},
		OutputSchema: packs.BasicSchema{
			Required: []string{"completed", "steps", "final_action"},
			Properties: map[string]string{
				"completed":    "boolean",
				"steps":        "number",
				"final_action": "object",
			},
		},
		Handler: visionClickAnywhereHandler(d),
	}
}

type visionClickAnywhereInput struct {
	Goal     string `json:"goal"`
	Model    string `json:"model"`
	MaxSteps int    `json:"max_steps"`
}

func visionClickAnywhereHandler(d vision.Dispatcher) packs.HandlerFunc {
	return func(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
		var in visionClickAnywhereInput
		if err := json.Unmarshal(ec.Input, &in); err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
		}
		if strings.TrimSpace(in.Goal) == "" {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "goal must not be empty"}
		}
		if strings.TrimSpace(in.Model) == "" {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "model must not be empty"}
		}
		if ec.Exec == nil {
			return nil, &packs.PackError{Code: packs.CodeSessionUnavailable, Message: "engine has no session executor"}
		}
		maxSteps := in.MaxSteps
		if maxSteps <= 0 {
			maxSteps = defaultVisionMaxSteps
		}
		ex := executorAdapter{ec: ec}

		// T807f: route to the native-tool-use path when the model's
		// provider supports it (Anthropic / OpenAI / Gemini); fall
		// back to the legacy JSON-prompt path otherwise (Ollama /
		// Deepseek / any future provider). The outer loop is the
		// same — one iteration per `vision.Step*` call — so only the
		// step function differs.
		stepFn := vision.Step
		if vision.SupportsNativeComputerUse(in.Model) {
			stepFn = vision.StepNative
		}

		var finalAction vision.Action
		stepsRun := 0
		completed := false
		for i := 0; i < maxSteps; i++ {
			step, err := stepFn(ctx, d, ex, ec.Session.ID, in.Model, in.Goal, defaultVisionMaxTokens)
			if err != nil {
				return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: err.Error(), Cause: err}
			}
			stepsRun++
			finalAction = step.Action
			if strings.EqualFold(step.Action.Action, "done") {
				completed = true
				break
			}
			if strings.EqualFold(step.Action.Action, "none") && i > 0 {
				// Two no-ops in a row means the model has nothing
				// useful to add — bail rather than burn the budget.
				break
			}
		}
		return json.Marshal(map[string]any{
			"completed":    completed,
			"steps":        stepsRun,
			"final_action": finalAction,
		})
	}
}

// VisionExtractVisibleText (T408) — pack interface for "transcribe
// every piece of visible text on the current screen". Single-step:
// take a screenshot, ask the model for a transcription, return the
// joined text. The Action JSON contract is reused: the model emits
// {"action":"done","reason":"...transcribed text..."} and we lift
// the reason field as the transcription.
//
// Input shape:
//
//	{
//	  "model": "openai/gpt-4o"  // required
//	}
//
// Output shape:
//
//	{
//	  "text":  "Username: ...\nPassword: ...",
//	  "model": "openai/gpt-4o"
//	}
func VisionExtractVisibleText(d vision.Dispatcher) *packs.Pack {
	return &packs.Pack{
		Name:        "vision.extract_visible_text",
		Version:     "v1",
		Description: "Transcribe all visible on-screen text using a vision model.",
		NeedsSession: true,
		SessionSpec: session.Spec{Env: map[string]string{"HELMDECK_MODE": "desktop"}},
		InputSchema: packs.BasicSchema{
			Required: []string{"model"},
			Properties: map[string]string{
				"model": "string",
			},
		},
		OutputSchema: packs.BasicSchema{
			Required: []string{"text", "model"},
			Properties: map[string]string{
				"text":  "string",
				"model": "string",
			},
		},
		Handler: visionExtractVisibleTextHandler(d),
	}
}

type visionExtractInput struct {
	Model string `json:"model"`
}

func visionExtractVisibleTextHandler(d vision.Dispatcher) packs.HandlerFunc {
	return func(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
		var in visionExtractInput
		if err := json.Unmarshal(ec.Input, &in); err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
		}
		if strings.TrimSpace(in.Model) == "" {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "model must not be empty"}
		}
		if ec.Exec == nil {
			return nil, &packs.PackError{Code: packs.CodeSessionUnavailable, Message: "engine has no session executor"}
		}
		ex := executorAdapter{ec: ec}
		png, err := vision.CaptureScreenshot(ctx, ex, ec.Session.ID)
		if err != nil {
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: err.Error(), Cause: err}
		}
		// For extraction we override the goal so the model knows to
		// transcribe rather than choose an action. The model will
		// still emit the Action JSON shape — we lift the Reason
		// field as the transcription text.
		raw, err := vision.AskModel(ctx, d, in.Model,
			"Transcribe all visible text on the screen. Return action=done with reason set to the full transcription, joined with newlines, no commentary.",
			png, defaultVisionMaxTokens)
		if err != nil {
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: err.Error(), Cause: err}
		}
		action, err := vision.ParseAction(raw)
		if err != nil {
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
				Message: fmt.Sprintf("could not parse model response: %v (raw: %q)", err, truncateString(raw, 256))}
		}
		return json.Marshal(map[string]any{
			"text":  action.Reason,
			"model": in.Model,
		})
	}
}

// VisionFillFormByLabel (T408) — pack interface for "fill out the
// form with these field/value pairs". Iterates the vision step loop,
// supplying the next pending field as the goal at each step, until
// every field has been entered or max_steps is hit.
//
// This is the messiest of the three packs because the action loop
// has to track which fields are still pending. We do it the simple
// way: each loop iteration sends a goal that names ONE pending
// field, and the model is responsible for clicking + typing one
// field's worth of action per iteration.
//
// Input shape:
//
//	{
//	  "model":     "openai/gpt-4o",
//	  "fields":    {"username": "alice", "password": "hunter2"},
//	  "max_steps": 12
//	}
//
// Output shape:
//
//	{
//	  "completed":      true,
//	  "fields_filled":  ["username","password"],
//	  "steps":          7
//	}
func VisionFillFormByLabel(d vision.Dispatcher) *packs.Pack {
	return &packs.Pack{
		Name:        "vision.fill_form_by_label",
		Version:     "v1",
		Description: "Fill a form on screen by matching label text, using a vision model.",
		NeedsSession: true,
		SessionSpec: session.Spec{Env: map[string]string{"HELMDECK_MODE": "desktop"}},
		InputSchema: packs.BasicSchema{
			Required: []string{"model", "fields"},
			Properties: map[string]string{
				"model":     "string",
				"fields":    "object",
				"max_steps": "number",
			},
		},
		OutputSchema: packs.BasicSchema{
			Required: []string{"completed", "fields_filled", "steps"},
			Properties: map[string]string{
				"completed":     "boolean",
				"fields_filled": "array",
				"steps":         "number",
			},
		},
		Handler: visionFillFormHandler(d),
	}
}

type visionFillFormInput struct {
	Model    string            `json:"model"`
	Fields   map[string]string `json:"fields"`
	MaxSteps int               `json:"max_steps"`
}

func visionFillFormHandler(d vision.Dispatcher) packs.HandlerFunc {
	return func(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
		var in visionFillFormInput
		if err := json.Unmarshal(ec.Input, &in); err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
		}
		if strings.TrimSpace(in.Model) == "" {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "model must not be empty"}
		}
		if len(in.Fields) == 0 {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "fields must contain at least one entry"}
		}
		if ec.Exec == nil {
			return nil, &packs.PackError{Code: packs.CodeSessionUnavailable, Message: "engine has no session executor"}
		}
		maxSteps := in.MaxSteps
		if maxSteps <= 0 {
			maxSteps = defaultVisionMaxSteps * 2 // forms typically take more steps than a single click
		}
		ex := executorAdapter{ec: ec}

		// Stable iteration order so the model sees fields in a
		// predictable sequence and the test fixtures stay
		// deterministic. Map iteration in Go is randomized.
		fieldNames := make([]string, 0, len(in.Fields))
		for k := range in.Fields {
			fieldNames = append(fieldNames, k)
		}
		// Sort alphabetically — the iteration order doesn't affect
		// correctness, only repeatability.
		sortStrings(fieldNames)

		filled := make([]string, 0, len(in.Fields))
		stepsRun := 0
		for _, name := range fieldNames {
			value := in.Fields[name]
			fieldGoal := fmt.Sprintf("Find the form field labeled %q and type %q into it. Return action=done when the field has been filled.", name, value)
			fieldDone := false
			for i := 0; i < maxSteps; i++ {
				step, err := vision.Step(ctx, d, ex, ec.Session.ID, in.Model, fieldGoal, defaultVisionMaxTokens)
				if err != nil {
					return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: err.Error(), Cause: err}
				}
				stepsRun++
				if strings.EqualFold(step.Action.Action, "done") {
					fieldDone = true
					break
				}
				if stepsRun >= maxSteps {
					break
				}
			}
			if fieldDone {
				filled = append(filled, name)
			}
			if stepsRun >= maxSteps {
				break
			}
		}
		return json.Marshal(map[string]any{
			"completed":     len(filled) == len(in.Fields),
			"fields_filled": filled,
			"steps":         stepsRun,
		})
	}
}

// executorAdapter wraps an ExecutionContext.Exec closure into a
// session.Executor so the vision package (which expects the full
// Executor interface) can be called from a pack handler. The
// closure already binds the session id, so we ignore the id arg.
type executorAdapter struct {
	ec *packs.ExecutionContext
}

func (a executorAdapter) Exec(ctx context.Context, _ string, req session.ExecRequest) (session.ExecResult, error) {
	return a.ec.Exec(ctx, req)
}

// truncateString is the local mirror of internal/api/vision.go's
// truncate helper. Duplicating five lines here is cheaper than
// exporting it across packages for one error message.
func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// sortStrings is a tiny stable insertion sort to avoid pulling in
// the sort package for one call site. Inputs are small (form field
// counts in the single digits) so O(n²) is fine.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}
