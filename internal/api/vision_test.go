package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/gateway"
	"github.com/tosin2013/helmdeck/internal/session"
	"github.com/tosin2013/helmdeck/internal/vision"
)

// stubDispatcher returns a canned reply and captures the request
// so tests can assert on the multimodal payload.
type stubDispatcher struct {
	got   gateway.ChatRequest
	reply string
	err   error
}

func (s *stubDispatcher) Dispatch(_ context.Context, req gateway.ChatRequest) (gateway.ChatResponse, error) {
	s.got = req
	if s.err != nil {
		return gateway.ChatResponse{}, s.err
	}
	return gateway.ChatResponse{
		Choices: []gateway.Choice{{
			Index:   0,
			Message: gateway.Message{Role: "assistant", Content: gateway.TextContent(s.reply)},
		}},
	}, nil
}

func newVisionMux(t *testing.T, rt session.Runtime, ex session.Executor, disp vision.Dispatcher) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sessions/{id}/vision/act", visionActHandler(rt, ex, disp))
	return mux
}

func TestVisionAct_HappyPathClick(t *testing.T) {
	pngBytes := []byte("\x89PNG\r\n\x1a\nfake")
	rt := stubRuntime{session: &session.Session{
		ID:          "sess-1",
		CDPEndpoint: "ws://h:9222/devtools/browser/x",
		Spec:        session.Spec{Env: map[string]string{"HELMDECK_MODE": "desktop"}},
	}}
	ex := &fakeExecutor{
		resultByCmd: map[string]session.ExecResult{
			"scrot":             {Stdout: pngBytes},
			"xdotool mousemove": {},
		},
	}
	disp := &stubDispatcher{reply: `{"action":"click","x":100,"y":200,"reason":"button"}`}
	h := newVisionMux(t, rt, ex, disp)

	body := `{"goal":"click submit","model":"openai/gpt-4o"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/vision/act", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp visionActResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Action.Action != "click" || resp.Action.X != 100 || resp.Action.Y != 200 {
		t.Errorf("action parsed wrong: %+v", resp.Action)
	}
	if !resp.Executed {
		t.Error("expected executed=true for click")
	}
	if resp.ScreenshotBytes != len(pngBytes) {
		t.Errorf("screenshot bytes wrong: %d", resp.ScreenshotBytes)
	}

	// Verify the dispatcher saw a multimodal user message with both
	// the goal text and the image data URL.
	userMsg := disp.got.Messages[1]
	if !userMsg.Content.IsMultipart() {
		t.Fatal("user message should be multipart")
	}
	imgs := userMsg.Content.Images()
	if len(imgs) != 1 || !strings.HasPrefix(imgs[0].URL, "data:image/png;base64,") {
		t.Errorf("user message missing data URL: %+v", imgs)
	}
}

func TestVisionAct_DoneActionNoOp(t *testing.T) {
	rt := stubRuntime{session: &session.Session{
		ID:          "s",
		CDPEndpoint: "ws://h:9222/x",
		Spec:        session.Spec{Env: map[string]string{"HELMDECK_MODE": "desktop"}},
	}}
	ex := &fakeExecutor{reply: session.ExecResult{Stdout: []byte("\x89PNG")}}
	disp := &stubDispatcher{reply: `{"action":"done","reason":"goal achieved"}`}
	h := newVisionMux(t, rt, ex, disp)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/s/vision/act",
		strings.NewReader(`{"goal":"x","model":"openai/gpt-4o"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var resp visionActResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Executed {
		t.Errorf("done should not execute side effects")
	}
}

func TestVisionAct_RejectsHeadlessSession(t *testing.T) {
	rt := stubRuntime{session: &session.Session{
		ID:   "s",
		Spec: session.Spec{Env: map[string]string{}},
	}}
	h := newVisionMux(t, rt, &fakeExecutor{}, &stubDispatcher{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/s/vision/act",
		strings.NewReader(`{"goal":"x","model":"openai/gpt-4o"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestVisionAct_MissingGoal(t *testing.T) {
	rt := stubRuntime{session: &session.Session{ID: "s", Spec: session.Spec{Env: map[string]string{"HELMDECK_MODE": "desktop"}}}}
	h := newVisionMux(t, rt, &fakeExecutor{}, &stubDispatcher{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/s/vision/act",
		strings.NewReader(`{"model":"openai/gpt-4o"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestVisionAct_ParseFailureBubbles(t *testing.T) {
	rt := stubRuntime{session: &session.Session{ID: "s", CDPEndpoint: "ws://h:9222/x", Spec: session.Spec{Env: map[string]string{"HELMDECK_MODE": "desktop"}}}}
	ex := &fakeExecutor{reply: session.ExecResult{Stdout: []byte("\x89PNG")}}
	disp := &stubDispatcher{reply: "I cannot help with that."}
	h := newVisionMux(t, rt, ex, disp)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/s/vision/act",
		strings.NewReader(`{"goal":"x","model":"openai/gpt-4o"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "model_parse_failed") {
		t.Errorf("expected model_parse_failed code: %s", rr.Body.String())
	}
}

func TestVisionAct_ModelErrorBubbles(t *testing.T) {
	rt := stubRuntime{session: &session.Session{ID: "s", CDPEndpoint: "ws://h:9222/x", Spec: session.Spec{Env: map[string]string{"HELMDECK_MODE": "desktop"}}}}
	ex := &fakeExecutor{reply: session.ExecResult{Stdout: []byte("\x89PNG")}}
	disp := &stubDispatcher{err: errors.New("rate limited")}
	h := newVisionMux(t, rt, ex, disp)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/s/vision/act",
		strings.NewReader(`{"goal":"x","model":"openai/gpt-4o"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "model_call_failed") {
		t.Errorf("expected model_call_failed code: %s", rr.Body.String())
	}
}
