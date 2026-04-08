package builtin

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
)

// PythonRun (per-pack image override demo, ADR 001) executes Python
// code or commands inside a Python-equipped session container. The
// pack acquires its own session via SessionSpec.Image, asking the
// runtime for the python sidecar instead of the default browser one.
//
// This is the canonical example of "Option B" per-pack image
// override: the pack catalog stays language-agnostic at the API
// layer, but each pack pins exactly the toolchain it needs at
// session-acquire time. Other languages (node.run, future
// rust.run, go.run, ...) follow the same pattern.
//
// Input shape (exactly one of code or command must be set):
//
//	{
//	  "code":    "print(2 + 2)",          // run inline via python3 -c
//	  "command": ["pytest", "-v"],         // OR run a command in cwd
//	  "cwd":     "/tmp/helmdeck-clone-X1", // optional working dir
//	  "stdin":   "input bytes"             // optional stdin
//	}
//
// Output shape:
//
//	{
//	  "stdout":    "...",
//	  "stderr":    "...",
//	  "exit_code": 0,
//	  "runtime":   "python"
//	}
func PythonRun() *packs.Pack {
	return &packs.Pack{
		Name:        "python.run",
		Version:     "v1",
		Description: "Run Python code or commands inside a Python-equipped session container.",
		NeedsSession: true,
		SessionSpec: session.Spec{
			Image: pythonSidecarImage(),
		},
		InputSchema: packs.BasicSchema{
			Properties: map[string]string{
				"code":    "string",
				"command": "array",
				"cwd":     "string",
				"stdin":   "string",
			},
		},
		OutputSchema: packs.BasicSchema{
			Required: []string{"stdout", "stderr", "exit_code", "runtime"},
			Properties: map[string]string{
				"stdout":    "string",
				"stderr":    "string",
				"exit_code": "number",
				"runtime":   "string",
			},
		},
		Handler: pythonRunHandler,
	}
}

type langRunInput struct {
	Code    string   `json:"code"`
	Command []string `json:"command"`
	Cwd     string   `json:"cwd"`
	Stdin   string   `json:"stdin"`
}

func pythonRunHandler(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
	var in langRunInput
	if err := json.Unmarshal(ec.Input, &in); err != nil {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
	}
	if err := validateLangRunInput(in); err != nil {
		return nil, err
	}
	if ec.Exec == nil {
		return nil, &packs.PackError{Code: packs.CodeSessionUnavailable, Message: "engine has no session executor"}
	}

	var cmd []string
	if strings.TrimSpace(in.Code) != "" {
		cmd = []string{"python3", "-c", in.Code}
	} else {
		cmd = append([]string{}, in.Command...)
	}

	res, err := runWithCwd(ctx, ec, cmd, in.Cwd, []byte(in.Stdin))
	if err != nil {
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: err.Error(), Cause: err}
	}
	return marshalLangRunResult(res, "python")
}

// validateLangRunInput enforces "exactly one of code or command".
// Both packs share this rule so the validator lives at package
// scope rather than being duplicated per language.
func validateLangRunInput(in langRunInput) *packs.PackError {
	hasCode := strings.TrimSpace(in.Code) != ""
	hasCmd := len(in.Command) > 0
	if hasCode == hasCmd {
		return &packs.PackError{Code: packs.CodeInvalidInput,
			Message: "exactly one of `code` or `command` must be set"}
	}
	return nil
}

// runWithCwd dispatches a command via ec.Exec, optionally wrapping
// it in a `sh -c` so the working directory takes effect. We avoid
// sh when no cwd is set so the captured argv stays inspectable in
// tests and there's one less layer of quoting to worry about.
func runWithCwd(ctx context.Context, ec *packs.ExecutionContext, cmd []string, cwd string, stdin []byte) (session.ExecResult, error) {
	if cwd == "" {
		return ec.Exec(ctx, session.ExecRequest{Cmd: cmd, Stdin: stdin})
	}
	quoted := make([]string, 0, len(cmd))
	for _, a := range cmd {
		quoted = append(quoted, shellQuote(a))
	}
	script := "cd " + shellQuote(cwd) + " && exec " + strings.Join(quoted, " ")
	return ec.Exec(ctx, session.ExecRequest{
		Cmd:   []string{"sh", "-c", script},
		Stdin: stdin,
	})
}

// marshalLangRunResult is the shared output encoder for language
// run packs. Keeps the field names and types in lockstep across
// python.run, node.run, and any future siblings.
func marshalLangRunResult(res session.ExecResult, runtime string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"stdout":    string(res.Stdout),
		"stderr":    string(res.Stderr),
		"exit_code": res.ExitCode,
		"runtime":   runtime,
	})
}

// pythonSidecarImage returns the image tag the pack pins via
// SessionSpec. Defaults to the canonical helmdeck Python sidecar;
// operators who roll their own per docs/SIDECAR-LANGUAGES.md can
// override by setting HELMDECK_SIDECAR_PYTHON in the control-plane
// environment before the binary starts.
func pythonSidecarImage() string {
	if v := os.Getenv("HELMDECK_SIDECAR_PYTHON"); v != "" {
		return v
	}
	return "ghcr.io/tosin2013/helmdeck-sidecar-python:latest"
}
