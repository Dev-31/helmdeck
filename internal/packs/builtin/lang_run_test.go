package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
)

// Shared engine constructor for the language-run pack tests.
func newLangEngine(t *testing.T, ex *recordingExecutor) *packs.Engine {
	t.Helper()
	return packs.New(
		packs.WithRuntime(fakeRuntime{}),
		packs.WithSessionExecutor(ex),
	)
}

// --- python.run ----------------------------------------------------------

func TestPythonRun_InlineCode(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("4\n"), ExitCode: 0},
	}}
	eng := newLangEngine(t, ex)
	res, err := eng.Execute(context.Background(), PythonRun(),
		json.RawMessage(`{"code":"print(2+2)"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(ex.calls))
	}
	cmd := ex.calls[0].Cmd
	if cmd[0] != "python3" || cmd[1] != "-c" || cmd[2] != "print(2+2)" {
		t.Errorf("argv = %v", cmd)
	}
	var out struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
		Runtime  string `json:"runtime"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.Stdout != "4\n" || out.ExitCode != 0 || out.Runtime != "python" {
		t.Errorf("output = %+v", out)
	}
}

func TestPythonRun_CommandWithCwd(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("ok"), ExitCode: 0},
	}}
	eng := newLangEngine(t, ex)
	_, err := eng.Execute(context.Background(), PythonRun(),
		json.RawMessage(`{"command":["pytest","-v"],"cwd":"/tmp/repo"}`))
	if err != nil {
		t.Fatal(err)
	}
	cmd := ex.calls[0].Cmd
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("expected sh -c wrapper, got %v", cmd)
	}
	script := cmd[2]
	if !strings.Contains(script, "cd '/tmp/repo'") {
		t.Errorf("cwd not in script: %s", script)
	}
	if !strings.Contains(script, "exec 'pytest' '-v'") {
		t.Errorf("argv not quoted in script: %s", script)
	}
}

func TestPythonRun_RejectsBothCodeAndCommand(t *testing.T) {
	eng := newLangEngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), PythonRun(),
		json.RawMessage(`{"code":"x","command":["y"]}`))
	if err == nil {
		t.Fatal("expected error when both code and command set")
	}
}

func TestPythonRun_RejectsNeither(t *testing.T) {
	eng := newLangEngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), PythonRun(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error when neither code nor command set")
	}
}

func TestPythonRun_NonZeroExitSurfacedNotErrored(t *testing.T) {
	// A failing command (e.g. failing test) is a NORMAL pack outcome,
	// not an error. The exit code lands in the output JSON.
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte(""), Stderr: []byte("AssertionError"), ExitCode: 1},
	}}
	eng := newLangEngine(t, ex)
	res, err := eng.Execute(context.Background(), PythonRun(),
		json.RawMessage(`{"code":"assert False"}`))
	if err != nil {
		t.Fatalf("non-zero exit should NOT be a pack error: %v", err)
	}
	var out struct {
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.ExitCode != 1 || !strings.Contains(out.Stderr, "AssertionError") {
		t.Errorf("non-zero exit not surfaced: %+v", out)
	}
}

func TestPythonRun_PinsPythonSidecarImage(t *testing.T) {
	p := PythonRun()
	if p.SessionSpec.Image == "" {
		t.Fatal("python.run must pin a SessionSpec.Image")
	}
	if !strings.Contains(p.SessionSpec.Image, "python") {
		t.Errorf("image should be the python sidecar, got %q", p.SessionSpec.Image)
	}
}

func TestPythonSidecarImage_EnvOverride(t *testing.T) {
	t.Setenv("HELMDECK_SIDECAR_PYTHON", "registry.example/custom-py:v1")
	if got := pythonSidecarImage(); got != "registry.example/custom-py:v1" {
		t.Errorf("env override not honored: %s", got)
	}
}

// --- node.run ------------------------------------------------------------

func TestNodeRun_InlineCode(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("4\n"), ExitCode: 0},
	}}
	eng := newLangEngine(t, ex)
	res, err := eng.Execute(context.Background(), NodeRun(),
		json.RawMessage(`{"code":"console.log(2+2)"}`))
	if err != nil {
		t.Fatal(err)
	}
	cmd := ex.calls[0].Cmd
	if cmd[0] != "node" || cmd[1] != "-e" || cmd[2] != "console.log(2+2)" {
		t.Errorf("argv = %v", cmd)
	}
	var out struct {
		Runtime string `json:"runtime"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.Runtime != "node" {
		t.Errorf("runtime = %q", out.Runtime)
	}
}

func TestNodeRun_CommandWithCwd(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("ok"), ExitCode: 0},
	}}
	eng := newLangEngine(t, ex)
	_, err := eng.Execute(context.Background(), NodeRun(),
		json.RawMessage(`{"command":["npm","test"],"cwd":"/tmp/proj"}`))
	if err != nil {
		t.Fatal(err)
	}
	script := ex.calls[0].Cmd[2]
	if !strings.Contains(script, "cd '/tmp/proj'") {
		t.Errorf("cwd not in script: %s", script)
	}
	if !strings.Contains(script, "exec 'npm' 'test'") {
		t.Errorf("npm test not in script: %s", script)
	}
}

func TestNodeRun_PinsNodeSidecarImage(t *testing.T) {
	p := NodeRun()
	if p.SessionSpec.Image == "" {
		t.Fatal("node.run must pin a SessionSpec.Image")
	}
	if !strings.Contains(p.SessionSpec.Image, "node") {
		t.Errorf("image should be the node sidecar, got %q", p.SessionSpec.Image)
	}
}

func TestNodeSidecarImage_EnvOverride(t *testing.T) {
	t.Setenv("HELMDECK_SIDECAR_NODE", "registry.example/custom-node:v1")
	if got := nodeSidecarImage(); got != "registry.example/custom-node:v1" {
		t.Errorf("env override not honored: %s", got)
	}
}

func TestLangRunPacksHaveDistinctImages(t *testing.T) {
	// The whole point of Option B is that two packs in the same
	// catalog can request different sidecar images. Catch any
	// regression that accidentally pins both to the same tag.
	if PythonRun().SessionSpec.Image == NodeRun().SessionSpec.Image {
		t.Errorf("python.run and node.run should pin different images")
	}
}
