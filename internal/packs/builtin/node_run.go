package builtin

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
)

// NodeRun (per-pack image override demo, ADR 001) executes Node.js
// code or commands inside a Node-equipped session container. Mirror
// of python.run with a different SessionSpec.Image and a different
// inline-code interpreter (`node -e <expr>`).
//
// Input shape (exactly one of code or command must be set):
//
//	{
//	  "code":    "console.log(2 + 2)",
//	  "command": ["npm", "test"],
//	  "cwd":     "/tmp/helmdeck-clone-X1",
//	  "stdin":   "input bytes"
//	}
//
// Output shape:
//
//	{
//	  "stdout":    "...",
//	  "stderr":    "...",
//	  "exit_code": 0,
//	  "runtime":   "node"
//	}
func NodeRun() *packs.Pack {
	return &packs.Pack{
		Name:        "node.run",
		Version:     "v1",
		Description: "Run Node.js code or commands inside a Node-equipped session container.",
		NeedsSession: true,
		SessionSpec: session.Spec{
			Image: nodeSidecarImage(),
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
		Handler: nodeRunHandler,
	}
}

func nodeRunHandler(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
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
		cmd = []string{"node", "-e", in.Code}
	} else {
		cmd = append([]string{}, in.Command...)
	}

	res, err := runWithCwd(ctx, ec, cmd, in.Cwd, []byte(in.Stdin))
	if err != nil {
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: err.Error(), Cause: err}
	}
	return marshalLangRunResult(res, "node")
}

// nodeSidecarImage returns the image tag for the Node sidecar.
// Override path: HELMDECK_SIDECAR_NODE — same convention as the
// Python pack.
func nodeSidecarImage() string {
	if v := os.Getenv("HELMDECK_SIDECAR_NODE"); v != "" {
		return v
	}
	return "ghcr.io/tosin2013/helmdeck-sidecar-node:latest"
}
