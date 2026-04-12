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
	"strconv"
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

// --- T807f: native computer-use tool-use path -------------------------
//
// Frontier providers (Anthropic, OpenAI, Google) now ship native
// computer-use tool schemas and their models are trained to emit
// them directly. Rather than keep routing every vision.* call
// through our bespoke JSON action format, we expose the provider's
// native schema via the gateway tool-use plumbing (T807f-A) and map
// the decoded tool call onto the existing xdotool/scrot dispatch
// that the legacy Step() path already uses.
//
// Design:
//   - ComputerUseAction is the internal representation — a superset
//     of the helmdeck Action struct covering every action the three
//     provider schemas define (click variants, drag, scroll, zoom,
//     wait, modifier clicks, etc.). Each provider parser translates
//     its wire format into this shape so DispatchComputerUseAction
//     only has to handle one internal type.
//   - BuildComputerUseTool returns a provider-specific ToolDefinition
//     whose InputSchema matches what the target model expects. For
//     Anthropic that's the computer_20251124 JSON Schema; for OpenAI
//     it's the function-tool wrapping; for Gemini it's the
//     normalized-coordinate shape the gemini-computer-use-preview
//     model emits.
//   - SupportsNativeComputerUse returns true iff the model's provider
//     prefix is one we've mapped. Ollama / Deepseek / anything else
//     fall back to the legacy prompt-based Step() path — this is the
//     documented "local model" fallback per the T807f plan.
//   - StepNative is the tool-use twin of Step(): one iteration of
//     (screenshot → model call with Tools[] → parse tool_use →
//     dispatch → return StepResult). The pack layer owns the outer
//     loop.
//
// The caller-visible Action struct is preserved unchanged so the
// old JSON-prompt path keeps working for Ollama and Deepseek. We do
// NOT deprecate it — it's the documented local-model fallback per
// the T807f plan decisions.

// ComputerUseAction mirrors the Claude computer_20251124 action
// vocabulary with a few Gemini / OpenAI extensions folded in. Every
// parser normalises into this shape; the dispatcher only reads it.
//
// Field conventions:
//   - Type is the canonical action name. We use Claude's names
//     because they're the broadest and best-documented set; OpenAI
//     and Gemini shapes translate onto them.
//   - X, Y are absolute pixel coordinates in the session's Xvfb
//     resolution. Gemini emits 0-1000 normalized coordinates; the
//     parser scales them before setting these fields.
//   - Button defaults to "left". "middle"/"right" are accepted.
//   - Modifiers is a lowercase list drawn from {shift,ctrl,alt,super}.
//   - Text is the string to type for Type actions, OR the
//     modifier name for click-with-modifier actions (Claude stuffs
//     a "text":"shift" into the click action when shift-click is
//     requested; we fold that into Modifiers during parsing).
//   - ScrollDirection is "up"/"down"/"left"/"right".
//   - Region is [x1, y1, x2, y2] for Zoom.
type ComputerUseAction struct {
	Type            string   `json:"type"`
	X               int      `json:"x,omitempty"`
	Y               int      `json:"y,omitempty"`
	EndX            int      `json:"end_x,omitempty"`
	EndY            int      `json:"end_y,omitempty"`
	Button          string   `json:"button,omitempty"`
	Modifiers       []string `json:"modifiers,omitempty"`
	Text            string   `json:"text,omitempty"`
	Keys            string   `json:"keys,omitempty"`
	ScrollDirection string   `json:"scroll_direction,omitempty"`
	ScrollAmount    int      `json:"scroll_amount,omitempty"`
	Region          [4]int   `json:"region,omitempty"`
	Seconds         float64  `json:"seconds,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

// ComputerUseToolName is the fixed tool name every provider's schema
// uses in its request body. Models are trained to emit this exact
// name; renaming it breaks the native path.
const ComputerUseToolName = "computer"

// nativeComputerUseProviders is the set of provider prefixes for
// which BuildComputerUseTool returns a real schema. Everything else
// falls back to the JSON-prompt path. Kept as a small set because
// each entry requires a tested parser — don't add a provider here
// without also adding the parsing path in parseComputerUseAction.
var nativeComputerUseProviders = map[string]bool{
	"anthropic": true,
	"openai":    true,
	"gemini":    true,
}

// SupportsNativeComputerUse reports whether the given `provider/model`
// string targets a provider whose native computer-use tool schema
// is wired up. Pack handlers call this to decide between StepNative
// (native path) and Step (legacy JSON-prompt path).
func SupportsNativeComputerUse(model string) bool {
	provider, _, err := gateway.SplitModel(model)
	if err != nil {
		return false
	}
	return nativeComputerUseProviders[provider]
}

// NativeComputerUseDescription is the frozen tool description every
// provider sees. Short because the model is trained to recognise
// this action space natively — our job is to give it a runtime,
// not re-explain what clicking means.
const NativeComputerUseDescription = "Interact with a Linux desktop. Emit one action per turn: screenshot, click, type, key, scroll, drag, wait, or done."

// anthropicComputerUseSchema is the InputSchema forwarded to Anthropic
// as the computer_20251124 tool's input_schema. It covers the action
// verbs Anthropic's reference implementation ships and maps 1:1 onto
// our ComputerUseAction struct so parseAnthropicComputerUse is a
// straight decode.
const anthropicComputerUseSchema = `{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["screenshot","left_click","right_click","middle_click","double_click","triple_click","type","key","mouse_move","scroll","left_click_drag","wait","zoom","done"]
    },
    "coordinate": {"type": "array", "items": {"type": "integer"}, "minItems": 2, "maxItems": 2},
    "start_coordinate": {"type": "array", "items": {"type": "integer"}, "minItems": 2, "maxItems": 2},
    "text": {"type": "string"},
    "scroll_direction": {"type": "string", "enum": ["up","down","left","right"]},
    "scroll_amount": {"type": "integer"},
    "region": {"type": "array", "items": {"type": "integer"}, "minItems": 4, "maxItems": 4},
    "duration": {"type": "number"},
    "reason": {"type": "string"}
  },
  "required": ["action"]
}`

// openAIComputerUseSchema matches OpenAI's function-tool shape. We
// use almost the same field names as Anthropic because OpenAI's
// computer-use-preview documentation closely follows Anthropic's
// and our parser reads both through the same code path.
const openAIComputerUseSchema = anthropicComputerUseSchema

// geminiComputerUseSchema uses Gemini's normalized 0-1000 coordinate
// space for x/y fields, which the parser scales to absolute pixels
// using the session's Xvfb resolution (1920x1080 by default — see
// deploy/docker/sidecar-entrypoint.sh). Gemini also prefers the
// snake-case action names it was trained on.
const geminiComputerUseSchema = `{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["screenshot","click_at","type_text_at","key","drag_and_drop","scroll_document","wait","done"]
    },
    "x": {"type": "integer", "description": "Normalized 0-1000 screen x"},
    "y": {"type": "integer", "description": "Normalized 0-1000 screen y"},
    "end_x": {"type": "integer"},
    "end_y": {"type": "integer"},
    "text": {"type": "string"},
    "direction": {"type": "string"},
    "seconds": {"type": "number"},
    "reason": {"type": "string"}
  },
  "required": ["action"]
}`

// BuildComputerUseTool produces the ToolDefinition for a given
// `provider/model` string. Returns the zero-value ToolDefinition +
// ok=false for unsupported providers so the caller knows to fall
// back to the JSON-prompt path.
func BuildComputerUseTool(model string) (gateway.ToolDefinition, bool) {
	provider, _, err := gateway.SplitModel(model)
	if err != nil {
		return gateway.ToolDefinition{}, false
	}
	var schema string
	switch provider {
	case "anthropic":
		schema = anthropicComputerUseSchema
	case "openai":
		schema = openAIComputerUseSchema
	case "gemini":
		schema = geminiComputerUseSchema
	default:
		return gateway.ToolDefinition{}, false
	}
	return gateway.ToolDefinition{
		Name:        ComputerUseToolName,
		Description: NativeComputerUseDescription,
		InputSchema: json.RawMessage(schema),
	}, true
}

// NativeSystemPrompt is the system message StepNative sends. Much
// shorter than the legacy JSON-prompt SystemPrompt because the
// model is trained to drive the tool directly — we just state the
// goal framing.
const NativeSystemPrompt = `You control a Linux desktop. Use the "computer" tool to interact. Emit ONE tool call per turn. Use the "done" action when the goal is achieved.`

// StepNative runs one iteration of the native-tool-use vision loop.
// Flow: capture screenshot → dispatch ChatRequest with Tools=[computer] →
// parse the model's tool_use block → translate to ComputerUseAction →
// execute via xdotool/scrot → return StepResult.
//
// Returns a wrapped error if the target model doesn't support
// native tool-use; callers should check SupportsNativeComputerUse
// first rather than relying on the error.
func StepNative(ctx context.Context, d Dispatcher, ex session.Executor, sessionID, model, goal string, maxTokens int) (StepResult, error) {
	tool, ok := BuildComputerUseTool(model)
	if !ok {
		return StepResult{}, fmt.Errorf("native computer-use not supported for model %q", model)
	}
	png, err := CaptureScreenshot(ctx, ex, sessionID)
	if err != nil {
		return StepResult{}, fmt.Errorf("screenshot: %w", err)
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	req := gateway.ChatRequest{
		Model:     model,
		MaxTokens: &maxTokens,
		Tools:     []gateway.ToolDefinition{tool},
		// Force a tool call on every turn so the model can't
		// degenerate into plain text when it's unsure. Pack layer
		// terminates the loop on the `done` action anyway.
		ToolChoice: &gateway.ToolChoice{Mode: "any"},
		Messages: []gateway.Message{
			{Role: "system", Content: gateway.TextContent(NativeSystemPrompt)},
			{Role: "user", Content: gateway.MultipartContent(
				gateway.TextPart("Goal: "+goal),
				gateway.ImageURLPartFromURL(dataURL),
			)},
		},
	}
	resp, err := d.Dispatch(ctx, req)
	if err != nil {
		return StepResult{Screenshot: png}, fmt.Errorf("model call: %w", err)
	}
	if len(resp.Choices) == 0 {
		return StepResult{Screenshot: png}, errors.New("model returned no choices")
	}
	// Find the first tool_use part in the assistant's response.
	// Multi-tool_use responses (rare but legal) are handled by
	// picking the first — the pack loop's next turn will pick up
	// the next action after this one lands.
	var toolUse *gateway.ContentPart
	for _, part := range resp.Choices[0].Message.Content.Parts() {
		if part.Type == gateway.ContentPartToolUse {
			p := part
			toolUse = &p
			break
		}
	}
	if toolUse == nil {
		// Model didn't emit a tool call even with ToolChoice=any —
		// surface the raw text for audit and bail. Pack layer will
		// treat this as a failed step.
		raw := resp.Choices[0].Message.Content.Text()
		return StepResult{
			ModelResponse: raw,
			Screenshot:    png,
		}, errors.New("model returned no tool_use block")
	}

	cu, perr := parseComputerUseAction(model, toolUse.ToolInput)
	if perr != nil {
		return StepResult{
			ModelResponse: string(toolUse.ToolInput),
			Screenshot:    png,
		}, fmt.Errorf("parse tool call: %w", perr)
	}

	// Translate ComputerUseAction → legacy Action for the returned
	// StepResult so callers that already consume StepResult.Action
	// keep working. The richer ComputerUseAction fields (drag, scroll,
	// region) aren't reflected in Action — the dispatcher uses the
	// full ComputerUseAction directly.
	action := Action{
		Action: cu.Type,
		X:      cu.X,
		Y:      cu.Y,
		Text:   cu.Text,
		Keys:   cu.Keys,
		Reason: cu.Reason,
	}
	executed, derr := DispatchComputerUseAction(ctx, ex, sessionID, cu)
	if derr != nil {
		return StepResult{
			Action:        action,
			ModelResponse: string(toolUse.ToolInput),
			Screenshot:    png,
		}, fmt.Errorf("dispatch: %w", derr)
	}
	return StepResult{
		Action:        action,
		Executed:      executed,
		ModelResponse: string(toolUse.ToolInput),
		Screenshot:    png,
	}, nil
}

// parseComputerUseAction decodes a provider-specific tool_use
// arguments JSON blob into the internal ComputerUseAction. Anthropic
// and OpenAI share a parser because their schemas we request are
// byte-identical; Gemini uses a separate path that scales the 0-1000
// normalized coordinates to pixel space.
func parseComputerUseAction(model string, input json.RawMessage) (ComputerUseAction, error) {
	provider, _, err := gateway.SplitModel(model)
	if err != nil {
		return ComputerUseAction{}, err
	}
	switch provider {
	case "anthropic", "openai":
		return parseAnthropicStyleComputerUse(input)
	case "gemini":
		return parseGeminiStyleComputerUse(input)
	default:
		return ComputerUseAction{}, fmt.Errorf("no parser for provider %q", provider)
	}
}

// anthropicStylePayload is the decoded shape of an Anthropic /
// OpenAI computer_20251124 tool_use input. Fields map 1:1 onto the
// schema declared in anthropicComputerUseSchema above.
type anthropicStylePayload struct {
	Action          string  `json:"action"`
	Coordinate      []int   `json:"coordinate,omitempty"`
	StartCoordinate []int   `json:"start_coordinate,omitempty"`
	Text            string  `json:"text,omitempty"`
	ScrollDirection string  `json:"scroll_direction,omitempty"`
	ScrollAmount    int     `json:"scroll_amount,omitempty"`
	Region          []int   `json:"region,omitempty"`
	Duration        float64 `json:"duration,omitempty"`
	Reason          string  `json:"reason,omitempty"`
}

// parseAnthropicStyleComputerUse maps the Claude/OpenAI tool_use
// arguments onto ComputerUseAction. The switch below enumerates
// every Claude computer_20251124 verb; unknown verbs error out so a
// hallucinated action surfaces as a step failure instead of silently
// no-op'ing.
func parseAnthropicStyleComputerUse(input json.RawMessage) (ComputerUseAction, error) {
	var p anthropicStylePayload
	if err := json.Unmarshal(input, &p); err != nil {
		return ComputerUseAction{}, fmt.Errorf("decode anthropic-style tool input: %w", err)
	}
	out := ComputerUseAction{Reason: p.Reason}
	// Coordinate fan-out — a lot of actions share the same [x, y]
	// shape, so unpack it once up front.
	if len(p.Coordinate) == 2 {
		out.X = p.Coordinate[0]
		out.Y = p.Coordinate[1]
	}
	switch p.Action {
	case "screenshot":
		out.Type = "screenshot"
	case "left_click":
		out.Type = "click"
		out.Button = "left"
		// Claude stuffs a modifier name into the "text" field
		// when a click should be held with a modifier (e.g.
		// "text":"shift"). Fold that into Modifiers; if it's
		// actual text for some unrelated action, we leave Text
		// populated too.
		if isModifierName(p.Text) {
			out.Modifiers = []string{strings.ToLower(p.Text)}
		} else {
			out.Text = p.Text
		}
	case "right_click":
		out.Type = "click"
		out.Button = "right"
	case "middle_click":
		out.Type = "click"
		out.Button = "middle"
	case "double_click":
		out.Type = "double_click"
		out.Button = "left"
	case "triple_click":
		out.Type = "triple_click"
		out.Button = "left"
	case "type":
		out.Type = "type"
		out.Text = p.Text
	case "key":
		out.Type = "key"
		out.Keys = p.Text
	case "mouse_move":
		out.Type = "mouse_move"
	case "scroll":
		out.Type = "scroll"
		out.ScrollDirection = p.ScrollDirection
		out.ScrollAmount = p.ScrollAmount
		if isModifierName(p.Text) {
			out.Modifiers = []string{strings.ToLower(p.Text)}
		}
	case "left_click_drag":
		out.Type = "drag"
		out.Button = "left"
		if len(p.StartCoordinate) == 2 {
			// Reinterpret: start_coordinate is the drag start,
			// coordinate is the drag end.
			out.X = p.StartCoordinate[0]
			out.Y = p.StartCoordinate[1]
		}
		if len(p.Coordinate) == 2 {
			out.EndX = p.Coordinate[0]
			out.EndY = p.Coordinate[1]
		}
	case "wait":
		out.Type = "wait"
		out.Seconds = p.Duration
	case "zoom":
		out.Type = "zoom"
		if len(p.Region) == 4 {
			out.Region = [4]int{p.Region[0], p.Region[1], p.Region[2], p.Region[3]}
		}
	case "done":
		out.Type = "done"
	default:
		return out, fmt.Errorf("unknown action %q", p.Action)
	}
	return out, nil
}

// geminiStylePayload decodes Gemini's computer-use-preview args.
// Gemini uses 0-1000 normalized coordinates — the parser scales
// them to pixels against the session's Xvfb resolution (default
// 1920x1080, set in sidecar-entrypoint.sh).
type geminiStylePayload struct {
	Action    string  `json:"action"`
	X         int     `json:"x,omitempty"`
	Y         int     `json:"y,omitempty"`
	EndX      int     `json:"end_x,omitempty"`
	EndY      int     `json:"end_y,omitempty"`
	Text      string  `json:"text,omitempty"`
	Direction string  `json:"direction,omitempty"`
	Seconds   float64 `json:"seconds,omitempty"`
	Reason    string  `json:"reason,omitempty"`
}

const (
	// Default Xvfb resolution from deploy/docker/sidecar-entrypoint.sh.
	// Gemini emits coordinates in a 0-1000 normalized space; we scale
	// by these dimensions when translating to absolute pixels.
	defaultSessionWidth  = 1920
	defaultSessionHeight = 1080
)

func scaleGeminiCoord(norm, extent int) int {
	if norm <= 0 {
		return 0
	}
	if norm >= 1000 {
		return extent - 1
	}
	return (norm * extent) / 1000
}

func parseGeminiStyleComputerUse(input json.RawMessage) (ComputerUseAction, error) {
	var p geminiStylePayload
	if err := json.Unmarshal(input, &p); err != nil {
		return ComputerUseAction{}, fmt.Errorf("decode gemini-style tool input: %w", err)
	}
	out := ComputerUseAction{
		Reason: p.Reason,
		X:      scaleGeminiCoord(p.X, defaultSessionWidth),
		Y:      scaleGeminiCoord(p.Y, defaultSessionHeight),
		EndX:   scaleGeminiCoord(p.EndX, defaultSessionWidth),
		EndY:   scaleGeminiCoord(p.EndY, defaultSessionHeight),
	}
	switch p.Action {
	case "screenshot":
		out.Type = "screenshot"
	case "click_at":
		out.Type = "click"
		out.Button = "left"
	case "type_text_at":
		out.Type = "type"
		out.Text = p.Text
	case "key":
		out.Type = "key"
		out.Keys = p.Text
	case "drag_and_drop":
		out.Type = "drag"
		out.Button = "left"
	case "scroll_document":
		out.Type = "scroll"
		out.ScrollDirection = p.Direction
		out.ScrollAmount = 3
	case "wait":
		out.Type = "wait"
		out.Seconds = p.Seconds
	case "done":
		out.Type = "done"
	default:
		return out, fmt.Errorf("unknown gemini action %q", p.Action)
	}
	return out, nil
}

// isModifierName reports whether s is one of the four xdotool
// modifier key names. Used to fold click-with-modifier requests
// from Claude's schema into ComputerUseAction.Modifiers.
func isModifierName(s string) bool {
	switch strings.ToLower(s) {
	case "shift", "ctrl", "control", "alt", "super", "meta":
		return true
	}
	return false
}

// DispatchComputerUseAction executes a ComputerUseAction against the
// session's Xvfb desktop via xdotool/scrot. Returns (executed, err);
// "done" / "screenshot" are no-ops that return executed=false
// because the loop itself handles those control-flow actions.
//
// This is the single-argv translator that every provider's tool_use
// eventually flows through, replacing the N-per-endpoint REST round
// trip. xdotool commands are composed inline so the whole action
// lands in one exec RPC.
func DispatchComputerUseAction(ctx context.Context, ex session.Executor, sessionID string, a ComputerUseAction) (bool, error) {
	switch strings.ToLower(a.Type) {
	case "", "none", "done", "screenshot":
		// Screenshot is a no-op at the dispatcher level because
		// the pack loop always captures a fresh screenshot on the
		// next iteration. Legacy Action's "none" / "" fall-through
		// is preserved for uniformity.
		return false, nil

	case "click":
		button := buttonCodeForName(a.Button)
		if len(a.Modifiers) > 0 {
			return runCmd(ctx, ex, sessionID, buildModifierClickArgv(a))
		}
		script := fmt.Sprintf("xdotool mousemove %d %d click %s", a.X, a.Y, button)
		return runCmd(ctx, ex, sessionID, []string{"sh", "-c", script})

	case "double_click":
		button := buttonCodeForName(a.Button)
		script := fmt.Sprintf("xdotool mousemove %d %d click --repeat 2 --delay 100 %s", a.X, a.Y, button)
		return runCmd(ctx, ex, sessionID, []string{"sh", "-c", script})

	case "triple_click":
		button := buttonCodeForName(a.Button)
		script := fmt.Sprintf("xdotool mousemove %d %d click --repeat 3 --delay 100 %s", a.X, a.Y, button)
		return runCmd(ctx, ex, sessionID, []string{"sh", "-c", script})

	case "drag":
		button := buttonCodeForName(a.Button)
		script := fmt.Sprintf("xdotool mousemove %d %d mousedown %s mousemove %d %d mouseup %s",
			a.X, a.Y, button, a.EndX, a.EndY, button)
		return runCmd(ctx, ex, sessionID, []string{"sh", "-c", script})

	case "scroll":
		var btn string
		switch strings.ToLower(a.ScrollDirection) {
		case "up":
			btn = "4"
		case "down", "":
			btn = "5"
		case "left":
			btn = "6"
		case "right":
			btn = "7"
		default:
			return false, fmt.Errorf("unknown scroll direction %q", a.ScrollDirection)
		}
		amt := a.ScrollAmount
		if amt <= 0 {
			amt = 3
		}
		if amt > 50 {
			amt = 50
		}
		var script string
		if a.X != 0 || a.Y != 0 {
			script = fmt.Sprintf("xdotool mousemove %d %d click --repeat %d %s", a.X, a.Y, amt, btn)
		} else {
			script = fmt.Sprintf("xdotool click --repeat %d %s", amt, btn)
		}
		return runCmd(ctx, ex, sessionID, []string{"sh", "-c", script})

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

	case "mouse_move":
		return runCmd(ctx, ex, sessionID, []string{"xdotool", "mousemove", strconv.Itoa(a.X), strconv.Itoa(a.Y)})

	case "wait":
		secs := a.Seconds
		if secs <= 0 {
			secs = 1
		}
		if secs > 30 {
			secs = 30
		}
		script := fmt.Sprintf("sleep %.3f", secs)
		return runCmd(ctx, ex, sessionID, []string{"sh", "-c", script})

	case "zoom":
		// Zoom is primarily a read action — it captures a region
		// and returns it as an image via the desktop REST endpoint.
		// Inside vision.StepNative the next iteration's screenshot
		// will reflect the new viewport anyway, so we treat zoom as
		// a no-op dispatcher-side. Packs that care about zoom
		// semantics should call the desktop REST /zoom endpoint
		// directly instead of going through the vision pipeline.
		return false, nil

	default:
		return false, fmt.Errorf("unknown computer-use action %q", a.Type)
	}
}

// buttonCodeForName returns the xdotool numeric button code for a
// human-readable name, defaulting to 1 (left).
func buttonCodeForName(name string) string {
	switch strings.ToLower(name) {
	case "middle":
		return "2"
	case "right":
		return "3"
	default:
		return "1"
	}
}

// buildModifierClickArgv composes a single xdotool argv that holds
// each modifier, clicks at (X, Y), and releases the modifiers.
// Mirrors the /api/v1/desktop/modifier_click endpoint's internal
// logic so the native path and the REST path emit byte-identical
// commands.
func buildModifierClickArgv(a ComputerUseAction) []string {
	button := buttonCodeForName(a.Button)
	argv := []string{"xdotool"}
	for _, m := range a.Modifiers {
		argv = append(argv, "keydown", strings.ToLower(m))
	}
	argv = append(argv, "mousemove", strconv.Itoa(a.X), strconv.Itoa(a.Y), "click", button)
	for _, m := range a.Modifiers {
		argv = append(argv, "keyup", strings.ToLower(m))
	}
	return argv
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
