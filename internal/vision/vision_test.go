// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package vision

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tosin2013/helmdeck/internal/gateway"
	"github.com/tosin2013/helmdeck/internal/session"
)

// --- test doubles ---------------------------------------------------------

// recordingExecutor captures every Exec call so tests can assert on
// the xdotool argv the dispatcher emitted.
type recordingExecutor struct {
	mu    sync.Mutex
	calls []session.ExecRequest
	reply session.ExecResult
	err   error
}

func (r *recordingExecutor) Exec(_ context.Context, _ string, req session.ExecRequest) (session.ExecResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	if r.err != nil {
		return session.ExecResult{}, r.err
	}
	return r.reply, nil
}

// execScript returns the sh -c script body the handler emitted, or
// the joined argv for direct xdotool invocations. Mirrors
// internal/api/desktop_test.go's helper of the same name.
func execScript(req session.ExecRequest) string {
	if len(req.Cmd) >= 3 && req.Cmd[0] == "sh" && req.Cmd[1] == "-c" {
		return req.Cmd[2]
	}
	return strings.Join(req.Cmd, " ")
}

// scriptedDispatcher returns a canned response on each Dispatch. The
// response's ContentPart shape is whatever the test constructs — we
// expose it as a field rather than a helper so each test builds the
// exact tool_use block it wants the parser to see.
type scriptedDispatcher struct {
	mu       sync.Mutex
	calls    int
	replies  []gateway.ChatResponse
	replyErr []error
}

func (s *scriptedDispatcher) Dispatch(_ context.Context, _ gateway.ChatRequest) (gateway.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.calls
	s.calls++
	if idx < len(s.replyErr) && s.replyErr[idx] != nil {
		return gateway.ChatResponse{}, s.replyErr[idx]
	}
	if idx < len(s.replies) {
		return s.replies[idx], nil
	}
	return gateway.ChatResponse{}, errors.New("scriptedDispatcher out of replies")
}

// scrotReply is a canned ExecResult that looks like scrot output for
// the screenshot hop inside StepNative. The bytes don't need to be a
// real PNG — we only encode them into a data URL and send them to the
// dispatcher stub.
var scrotReply = session.ExecResult{Stdout: []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}}

// newScriptedExecutor returns an executor that replies to every Exec
// with scrotReply (so screenshot hops succeed) except the last call,
// which is where the actual dispatched action lands. Tests assert on
// that last call's argv to verify the dispatcher did the right thing.
func newScriptedExecutor() *recordingExecutor {
	return &recordingExecutor{reply: scrotReply}
}

// buildToolUseResponse constructs a ChatResponse containing one
// assistant-role message with a single tool_use block carrying the
// given input JSON. Used by every StepNative test.
func buildToolUseResponse(toolName, id string, input string) gateway.ChatResponse {
	return gateway.ChatResponse{
		Choices: []gateway.Choice{{
			Index: 0,
			Message: gateway.Message{
				Role: "assistant",
				Content: gateway.MultipartContent(
					gateway.ToolUsePart(id, toolName, json.RawMessage(input)),
				),
			},
		}},
	}
}

// --- SupportsNativeComputerUse -------------------------------------------

func TestSupportsNativeComputerUse(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"anthropic/claude-opus-4-6", true},
		{"openai/gpt-4o", true},
		{"gemini/gemini-3-flash-preview", true},
		{"ollama/llama3.2", false},
		{"deepseek/deepseek-chat", false},
		{"not-a-model", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := SupportsNativeComputerUse(tc.model); got != tc.want {
				t.Errorf("SupportsNativeComputerUse(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

// --- BuildComputerUseTool ------------------------------------------------

func TestBuildComputerUseTool_Anthropic(t *testing.T) {
	tool, ok := BuildComputerUseTool("anthropic/claude-opus-4-6")
	if !ok {
		t.Fatal("expected ok=true for anthropic")
	}
	if tool.Name != ComputerUseToolName {
		t.Errorf("name = %q, want %q", tool.Name, ComputerUseToolName)
	}
	// Schema should mention the canonical action verbs.
	schema := string(tool.InputSchema)
	for _, verb := range []string{"left_click", "double_click", "scroll", "zoom", "wait"} {
		if !strings.Contains(schema, verb) {
			t.Errorf("anthropic schema missing %q verb", verb)
		}
	}
}

func TestBuildComputerUseTool_Gemini(t *testing.T) {
	tool, ok := BuildComputerUseTool("gemini/gemini-3-flash-preview")
	if !ok {
		t.Fatal("expected ok=true for gemini")
	}
	schema := string(tool.InputSchema)
	// Gemini-specific verb names
	for _, verb := range []string{"click_at", "type_text_at", "scroll_document", "drag_and_drop"} {
		if !strings.Contains(schema, verb) {
			t.Errorf("gemini schema missing %q verb", verb)
		}
	}
}

func TestBuildComputerUseTool_UnsupportedProvider(t *testing.T) {
	_, ok := BuildComputerUseTool("ollama/llama3.2")
	if ok {
		t.Error("ollama should not produce a tool")
	}
	_, ok = BuildComputerUseTool("bogus")
	if ok {
		t.Error("invalid model should not produce a tool")
	}
}

// --- parseAnthropicStyleComputerUse --------------------------------------

func TestParseAnthropicStyleComputerUse_AllActions(t *testing.T) {
	cases := []struct {
		name  string
		input string
		check func(t *testing.T, a ComputerUseAction)
	}{
		{
			"left_click",
			`{"action":"left_click","coordinate":[100,200],"reason":"click button"}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "click" || a.Button != "left" || a.X != 100 || a.Y != 200 {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"left_click with shift modifier",
			`{"action":"left_click","coordinate":[50,50],"text":"shift"}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "click" || len(a.Modifiers) != 1 || a.Modifiers[0] != "shift" {
					t.Errorf("modifier fold failed: %+v", a)
				}
			},
		},
		{
			"right_click",
			`{"action":"right_click","coordinate":[1,2]}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "click" || a.Button != "right" {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"double_click",
			`{"action":"double_click","coordinate":[10,20]}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "double_click" || a.X != 10 || a.Y != 20 {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"triple_click",
			`{"action":"triple_click","coordinate":[30,40]}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "triple_click" {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"type",
			`{"action":"type","text":"hello world"}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "type" || a.Text != "hello world" {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"key",
			`{"action":"key","text":"ctrl+a"}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "key" || a.Keys != "ctrl+a" {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"mouse_move",
			`{"action":"mouse_move","coordinate":[99,88]}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "mouse_move" || a.X != 99 || a.Y != 88 {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"scroll down 5",
			`{"action":"scroll","coordinate":[100,100],"scroll_direction":"down","scroll_amount":5}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "scroll" || a.ScrollDirection != "down" || a.ScrollAmount != 5 {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"left_click_drag",
			`{"action":"left_click_drag","start_coordinate":[10,20],"coordinate":[100,200]}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "drag" || a.X != 10 || a.Y != 20 || a.EndX != 100 || a.EndY != 200 {
					t.Errorf("drag coords wrong: %+v", a)
				}
			},
		},
		{
			"wait",
			`{"action":"wait","duration":2.5}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "wait" || a.Seconds != 2.5 {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"zoom",
			`{"action":"zoom","region":[10,20,110,220]}`,
			func(t *testing.T, a ComputerUseAction) {
				want := [4]int{10, 20, 110, 220}
				if a.Type != "zoom" || a.Region != want {
					t.Errorf("got %+v", a)
				}
			},
		},
		{
			"done",
			`{"action":"done","reason":"all good"}`,
			func(t *testing.T, a ComputerUseAction) {
				if a.Type != "done" || a.Reason != "all good" {
					t.Errorf("got %+v", a)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAnthropicStyleComputerUse(json.RawMessage(tc.input))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			tc.check(t, got)
		})
	}
}

func TestParseAnthropicStyleComputerUse_UnknownAction(t *testing.T) {
	_, err := parseAnthropicStyleComputerUse(json.RawMessage(`{"action":"levitate"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("want unknown action error, got %v", err)
	}
}

// --- parseGeminiStyleComputerUse -----------------------------------------

func TestParseGeminiStyleComputerUse_ScalesCoordinates(t *testing.T) {
	// Gemini emits 0-1000 normalized; parser scales to 1920x1080.
	got, err := parseGeminiStyleComputerUse(json.RawMessage(
		`{"action":"click_at","x":500,"y":500}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "click" {
		t.Errorf("type = %q, want click", got.Type)
	}
	// 500/1000 * 1920 = 960, 500/1000 * 1080 = 540
	if got.X != 960 || got.Y != 540 {
		t.Errorf("coords = (%d,%d), want (960,540)", got.X, got.Y)
	}
}

func TestParseGeminiStyleComputerUse_EdgeCoordinates(t *testing.T) {
	// 0 stays 0; 1000 maps to extent-1 so clicks don't land off-screen.
	got, err := parseGeminiStyleComputerUse(json.RawMessage(
		`{"action":"click_at","x":0,"y":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.X != 0 {
		t.Errorf("x=0 should stay 0, got %d", got.X)
	}
	if got.Y != 1079 {
		t.Errorf("y=1000 should clamp to 1079 (1080-1), got %d", got.Y)
	}
}

func TestParseGeminiStyleComputerUse_DragAndDrop(t *testing.T) {
	got, err := parseGeminiStyleComputerUse(json.RawMessage(
		`{"action":"drag_and_drop","x":100,"y":100,"end_x":500,"end_y":500}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "drag" {
		t.Errorf("type = %q, want drag", got.Type)
	}
	// 100/1000 * 1920 = 192, 500/1000 * 1920 = 960, etc.
	if got.X != 192 || got.EndX != 960 {
		t.Errorf("x/end_x = (%d,%d)", got.X, got.EndX)
	}
}

// --- DispatchComputerUseAction -------------------------------------------

func TestDispatchComputerUseAction_Click(t *testing.T) {
	ex := newScriptedExecutor()
	ex.reply = session.ExecResult{}
	_, err := DispatchComputerUseAction(context.Background(), ex, "s",
		ComputerUseAction{Type: "click", Button: "left", X: 100, Y: 200})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	body := execScript(ex.calls[0])
	if !strings.Contains(body, "mousemove 100 200") || !strings.Contains(body, "click 1") {
		t.Errorf("wrong argv: %s", body)
	}
}

func TestDispatchComputerUseAction_ModifierClick(t *testing.T) {
	ex := newScriptedExecutor()
	ex.reply = session.ExecResult{}
	_, err := DispatchComputerUseAction(context.Background(), ex, "s",
		ComputerUseAction{Type: "click", X: 50, Y: 50, Modifiers: []string{"shift"}})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Modifier click goes through argv (not sh -c), so inspect Cmd directly.
	joined := strings.Join(ex.calls[0].Cmd, " ")
	for _, want := range []string{"keydown shift", "mousemove 50 50", "click 1", "keyup shift"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q: %s", want, joined)
		}
	}
}

func TestDispatchComputerUseAction_Drag(t *testing.T) {
	ex := newScriptedExecutor()
	ex.reply = session.ExecResult{}
	_, err := DispatchComputerUseAction(context.Background(), ex, "s",
		ComputerUseAction{Type: "drag", Button: "left", X: 10, Y: 20, EndX: 100, EndY: 200})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	body := execScript(ex.calls[0])
	for _, want := range []string{"mousemove 10 20", "mousedown 1", "mousemove 100 200", "mouseup 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("drag missing %q: %s", want, body)
		}
	}
}

func TestDispatchComputerUseAction_Scroll(t *testing.T) {
	cases := []struct {
		name   string
		a      ComputerUseAction
		button string
	}{
		{"down", ComputerUseAction{Type: "scroll", ScrollDirection: "down", ScrollAmount: 3}, "5"},
		{"up", ComputerUseAction{Type: "scroll", ScrollDirection: "up", ScrollAmount: 1}, "4"},
		{"left", ComputerUseAction{Type: "scroll", ScrollDirection: "left", ScrollAmount: 2}, "6"},
		{"right", ComputerUseAction{Type: "scroll", ScrollDirection: "right", ScrollAmount: 2}, "7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := newScriptedExecutor()
			ex.reply = session.ExecResult{}
			_, err := DispatchComputerUseAction(context.Background(), ex, "s", tc.a)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			body := execScript(ex.calls[0])
			if !strings.Contains(body, " "+tc.button) {
				t.Errorf("expected button %s in %s: %s", tc.button, tc.name, body)
			}
		})
	}
}

func TestDispatchComputerUseAction_Type(t *testing.T) {
	ex := newScriptedExecutor()
	ex.reply = session.ExecResult{}
	_, err := DispatchComputerUseAction(context.Background(), ex, "s",
		ComputerUseAction{Type: "type", Text: "hello"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	joined := strings.Join(ex.calls[0].Cmd, " ")
	if !strings.Contains(joined, "xdotool type -- hello") {
		t.Errorf("wrong type argv: %s", joined)
	}
}

func TestDispatchComputerUseAction_Key(t *testing.T) {
	ex := newScriptedExecutor()
	ex.reply = session.ExecResult{}
	_, err := DispatchComputerUseAction(context.Background(), ex, "s",
		ComputerUseAction{Type: "key", Keys: "Return"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	joined := strings.Join(ex.calls[0].Cmd, " ")
	if !strings.Contains(joined, "xdotool key -- Return") {
		t.Errorf("wrong key argv: %s", joined)
	}
}

func TestDispatchComputerUseAction_Wait(t *testing.T) {
	ex := newScriptedExecutor()
	ex.reply = session.ExecResult{}
	_, err := DispatchComputerUseAction(context.Background(), ex, "s",
		ComputerUseAction{Type: "wait", Seconds: 2.5})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	body := execScript(ex.calls[0])
	if !strings.Contains(body, "sleep 2.500") {
		t.Errorf("wrong sleep: %s", body)
	}
}

func TestDispatchComputerUseAction_WaitCappedAt30(t *testing.T) {
	ex := newScriptedExecutor()
	ex.reply = session.ExecResult{}
	_, err := DispatchComputerUseAction(context.Background(), ex, "s",
		ComputerUseAction{Type: "wait", Seconds: 9999})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	body := execScript(ex.calls[0])
	if !strings.Contains(body, "sleep 30.000") {
		t.Errorf("wait should cap at 30: %s", body)
	}
}

func TestDispatchComputerUseAction_ControlFlowActionsNoop(t *testing.T) {
	// done, screenshot, "", none — all return executed=false and
	// make no exec calls.
	for _, typ := range []string{"done", "screenshot", "", "none"} {
		t.Run(typ, func(t *testing.T) {
			ex := newScriptedExecutor()
			ex.reply = session.ExecResult{}
			executed, err := DispatchComputerUseAction(context.Background(), ex, "s",
				ComputerUseAction{Type: typ})
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if executed {
				t.Errorf("%q should not execute", typ)
			}
			if len(ex.calls) != 0 {
				t.Errorf("%q should not dispatch, got %d calls", typ, len(ex.calls))
			}
		})
	}
}

func TestDispatchComputerUseAction_UnknownType(t *testing.T) {
	ex := newScriptedExecutor()
	ex.reply = session.ExecResult{}
	_, err := DispatchComputerUseAction(context.Background(), ex, "s",
		ComputerUseAction{Type: "levitate"})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("want unknown error, got %v", err)
	}
}

// --- StepNative end-to-end ------------------------------------------------

func TestStepNative_HappyPath_Anthropic(t *testing.T) {
	// Executor replies with scrot bytes on call 0 (screenshot), then
	// success on call 1 (the click dispatched after the model's
	// tool_use is parsed).
	ex := &recordingExecutor{}
	callCount := 0
	ex.reply = scrotReply // default — screenshot gets this
	// We need a per-call reply: screenshot returns PNG bytes,
	// click returns zero-value success. Swap by wrapping.
	origReply := ex.reply
	_ = origReply
	// Use a closure pattern: the first Exec call is the screenshot
	// (returns scrot bytes), subsequent calls are the dispatch
	// (return empty success). We implement this via a tiny stateful
	// wrapper.
	ex = &recordingExecutor{}
	statefulEx := &statefulExecutor{base: ex, screenshotReply: scrotReply, actionReply: session.ExecResult{}}
	_ = callCount

	disp := &scriptedDispatcher{
		replies: []gateway.ChatResponse{
			buildToolUseResponse(ComputerUseToolName, "toolu_01",
				`{"action":"left_click","coordinate":[150,250],"reason":"target located"}`),
		},
	}
	step, err := StepNative(context.Background(), disp, statefulEx, "sess-1",
		"anthropic/claude-opus-4-6", "click the button", 512)
	if err != nil {
		t.Fatalf("StepNative: %v", err)
	}
	if step.Action.Action != "click" || step.Action.X != 150 || step.Action.Y != 250 {
		t.Errorf("action wrong: %+v", step.Action)
	}
	if !step.Executed {
		t.Error("expected Executed=true for click")
	}
	if len(step.Screenshot) == 0 {
		t.Error("screenshot bytes not returned")
	}
	// Two exec calls: one screenshot, one click.
	if len(ex.calls) != 2 {
		t.Errorf("exec call count = %d, want 2", len(ex.calls))
	}
	clickCall := execScript(ex.calls[1])
	if !strings.Contains(clickCall, "mousemove 150 250") {
		t.Errorf("click argv wrong: %s", clickCall)
	}
}

func TestStepNative_HappyPath_OpenAI(t *testing.T) {
	ex := &recordingExecutor{}
	statefulEx := &statefulExecutor{base: ex, screenshotReply: scrotReply, actionReply: session.ExecResult{}}
	disp := &scriptedDispatcher{
		replies: []gateway.ChatResponse{
			buildToolUseResponse(ComputerUseToolName, "call_abc",
				`{"action":"type","text":"hello world"}`),
		},
	}
	step, err := StepNative(context.Background(), disp, statefulEx, "sess-1",
		"openai/gpt-4o", "type greeting", 512)
	if err != nil {
		t.Fatalf("StepNative: %v", err)
	}
	if step.Action.Action != "type" || step.Action.Text != "hello world" {
		t.Errorf("got %+v", step.Action)
	}
}

func TestStepNative_HappyPath_Gemini(t *testing.T) {
	ex := &recordingExecutor{}
	statefulEx := &statefulExecutor{base: ex, screenshotReply: scrotReply, actionReply: session.ExecResult{}}
	disp := &scriptedDispatcher{
		replies: []gateway.ChatResponse{
			buildToolUseResponse(ComputerUseToolName, "gemini-call-click_at",
				`{"action":"click_at","x":500,"y":500}`),
		},
	}
	step, err := StepNative(context.Background(), disp, statefulEx, "sess-1",
		"gemini/gemini-3-flash-preview", "click center", 512)
	if err != nil {
		t.Fatalf("StepNative: %v", err)
	}
	// Gemini 500/1000 → 960 / 540 in 1920x1080.
	if step.Action.X != 960 || step.Action.Y != 540 {
		t.Errorf("gemini scaling wrong: %+v", step.Action)
	}
}

func TestStepNative_UnsupportedProvider(t *testing.T) {
	disp := &scriptedDispatcher{}
	ex := &recordingExecutor{reply: scrotReply}
	_, err := StepNative(context.Background(), disp, ex, "s",
		"ollama/llama3.2", "goal", 512)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("want not supported error, got %v", err)
	}
}

func TestStepNative_NoToolUseBlock(t *testing.T) {
	// Model returned plain text instead of a tool_use — surfaces
	// as an error so the pack layer records the failed step.
	ex := &recordingExecutor{reply: scrotReply}
	disp := &scriptedDispatcher{
		replies: []gateway.ChatResponse{
			{Choices: []gateway.Choice{{
				Index:   0,
				Message: gateway.Message{Role: "assistant", Content: gateway.TextContent("I'll click it")},
			}}},
		},
	}
	step, err := StepNative(context.Background(), disp, ex, "s",
		"anthropic/claude-opus-4-6", "goal", 512)
	if err == nil || !strings.Contains(err.Error(), "no tool_use") {
		t.Errorf("want no tool_use error, got %v", err)
	}
	// Screenshot still captured for debugging.
	if len(step.Screenshot) == 0 {
		t.Error("screenshot should still be returned on parse failure")
	}
}

func TestStepNative_ScreenshotFails(t *testing.T) {
	ex := &recordingExecutor{err: errors.New("scrot exploded")}
	disp := &scriptedDispatcher{}
	_, err := StepNative(context.Background(), disp, ex, "s",
		"anthropic/claude-opus-4-6", "goal", 512)
	if err == nil || !strings.Contains(err.Error(), "screenshot") {
		t.Errorf("want screenshot error, got %v", err)
	}
}

func TestStepNative_DispatcherFails(t *testing.T) {
	ex := &recordingExecutor{reply: scrotReply}
	disp := &scriptedDispatcher{replyErr: []error{errors.New("provider 502")}}
	_, err := StepNative(context.Background(), disp, ex, "s",
		"anthropic/claude-opus-4-6", "goal", 512)
	if err == nil || !strings.Contains(err.Error(), "model call") {
		t.Errorf("want model call error, got %v", err)
	}
}

// statefulExecutor is a tiny wrapper around recordingExecutor that
// returns different canned replies depending on whether the call
// looks like a screenshot (scrot) or an action dispatch. Factored
// out so the happy-path tests above can stub the two-call
// (screenshot → dispatch) sequence StepNative always does.
type statefulExecutor struct {
	base            *recordingExecutor
	screenshotReply session.ExecResult
	actionReply     session.ExecResult
}

func (s *statefulExecutor) Exec(ctx context.Context, id string, req session.ExecRequest) (session.ExecResult, error) {
	s.base.mu.Lock()
	s.base.calls = append(s.base.calls, req)
	s.base.mu.Unlock()
	if len(req.Cmd) >= 3 && req.Cmd[0] == "sh" && req.Cmd[1] == "-c" &&
		strings.Contains(req.Cmd[2], "scrot") {
		return s.screenshotReply, nil
	}
	return s.actionReply, nil
}
