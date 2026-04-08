// Package api — vision-mode action endpoint (T407, ADR 027).
//
// POST /api/v1/sessions/{id}/vision/act takes a high-level goal in
// natural language ("click the submit button"), captures a screenshot
// of the session container's desktop, sends image + goal through the
// AI gateway with multimodal content, parses the model's structured
// action JSON response, and dispatches the action via the existing
// session.Executor against xdotool/scrot.
//
// The substantive logic lives in internal/vision/ so the reference
// vision packs (T408) can share the same screenshot → model → parse
// → dispatch pipeline without an api ↔ packs/builtin import cycle.
// This file is a thin HTTP veneer that handles request validation
// and response shaping.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tosin2013/helmdeck/internal/gateway"
	"github.com/tosin2013/helmdeck/internal/session"
	"github.com/tosin2013/helmdeck/internal/vision"
)

type visionActRequest struct {
	Goal      string `json:"goal"`
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

type visionActResponse struct {
	Action          vision.Action `json:"action"`
	Executed        bool          `json:"executed"`
	ModelResponse   string        `json:"model_response"`
	ScreenshotBytes int           `json:"screenshot_bytes"`
}

func registerVisionRoutes(mux *http.ServeMux, deps Deps) {
	if deps.Runtime == nil || deps.Executor == nil {
		mux.HandleFunc("POST /api/v1/sessions/{id}/vision/act", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "vision_unavailable",
				"vision actions require both a session runtime and a session.Executor")
		})
		return
	}
	// Pick the chain when present (it includes fallback rules); fall
	// back to the bare registry. Same precedence as gateway.go.
	var dispatcher vision.Dispatcher
	if deps.GatewayChain != nil {
		dispatcher = deps.GatewayChain
	} else if deps.Gateway != nil {
		dispatcher = deps.Gateway
	}
	if dispatcher == nil {
		mux.HandleFunc("POST /api/v1/sessions/{id}/vision/act", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "vision_unavailable",
				"vision actions require an AI gateway provider")
		})
		return
	}
	mux.HandleFunc("POST /api/v1/sessions/{id}/vision/act",
		visionActHandler(deps.Runtime, deps.Executor, dispatcher))
}

// visionActHandler is the HTTP handler factored out so tests can
// inject a stub dispatcher without constructing a real
// *gateway.Registry. The production registerVisionRoutes wires the
// dispatcher from Deps; tests pass their own.
func visionActHandler(rt session.Runtime, ex session.Executor, dispatcher vision.Dispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if sessionID == "" {
			writeError(w, http.StatusBadRequest, "missing_session_id", "session id is required")
			return
		}
		var req visionActRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if strings.TrimSpace(req.Goal) == "" {
			writeError(w, http.StatusBadRequest, "missing_goal", "goal is required")
			return
		}
		if strings.TrimSpace(req.Model) == "" {
			writeError(w, http.StatusBadRequest, "missing_model",
				"model is required (provider/model syntax, e.g. openai/gpt-4o)")
			return
		}
		sess, err := rt.Get(r.Context(), sessionID)
		if err != nil {
			if errors.Is(err, session.ErrSessionNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			writeError(w, http.StatusBadGateway, "runtime_failed", err.Error())
			return
		}
		if sess.Spec.Env["HELMDECK_MODE"] != "desktop" {
			writeError(w, http.StatusBadRequest, "not_desktop_mode",
				"vision actions require a session created with HELMDECK_MODE=desktop")
			return
		}

		step, err := vision.Step(r.Context(), dispatcher, ex, sessionID, req.Model, req.Goal, req.MaxTokens)
		if err != nil {
			// Surface a code that hints at which stage failed. The
			// step error wrapping uses fmt.Errorf with prefixes
			// "screenshot:", "model call:", "parse action:", and
			// "dispatch action:" — match those for the typed code.
			msg := err.Error()
			code := "vision_failed"
			switch {
			case strings.HasPrefix(msg, "screenshot:"):
				code = "screenshot_failed"
			case strings.HasPrefix(msg, "model call:"):
				code = "model_call_failed"
			case strings.HasPrefix(msg, "parse action:"):
				code = "model_parse_failed"
			case strings.HasPrefix(msg, "dispatch action:"):
				code = "action_failed"
			}
			writeError(w, http.StatusBadGateway, code, fmt.Sprintf("%v", err))
			return
		}

		writeJSON(w, http.StatusOK, visionActResponse{
			Action:          step.Action,
			Executed:        step.Executed,
			ModelResponse:   step.ModelResponse,
			ScreenshotBytes: len(step.Screenshot),
		})
	}
}

// Compile-time assertion that *gateway.Registry and *gateway.Chain
// satisfy the vision.Dispatcher interface — keeps the precedence
// switch above honest if either type's signature drifts.
var (
	_ vision.Dispatcher = (*gateway.Registry)(nil)
	_ vision.Dispatcher = (*gateway.Chain)(nil)
)
