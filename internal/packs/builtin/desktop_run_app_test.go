package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
)

// recordingExecutor captures every Exec call so tests can assert on
// the launch / screenshot argv pair the desktop pack emits.
type recordingExecutor struct {
	calls   []session.ExecRequest
	replies []session.ExecResult
}

func (r *recordingExecutor) Exec(ctx context.Context, id string, req session.ExecRequest) (session.ExecResult, error) {
	r.calls = append(r.calls, req)
	idx := len(r.calls) - 1
	if idx < len(r.replies) {
		return r.replies[idx], nil
	}
	return session.ExecResult{}, nil
}

func newDesktopEngine(t *testing.T, ex *recordingExecutor) *packs.Engine {
	t.Helper()
	return packs.New(
		packs.WithRuntime(fakeRuntime{}),
		packs.WithSessionExecutor(ex),
	)
}

func TestDesktopRunAppHappyPath(t *testing.T) {
	pngBytes := []byte("\x89PNG\r\n\x1a\nfake")
	ex := &recordingExecutor{
		// First call: launch (empty stdout). Second call: scrot output.
		replies: []session.ExecResult{
			{},
			{Stdout: pngBytes},
		},
	}
	eng := newDesktopEngine(t, ex)

	res, err := eng.Execute(context.Background(), DesktopRunAppAndScreenshot(),
		json.RawMessage(`{"command":"xterm","args":["-e","echo hi"],"wait_ms":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ex.calls) != 2 {
		t.Fatalf("expected 2 exec calls (launch + scrot), got %d", len(ex.calls))
	}
	launchScript := strings.Join(ex.calls[0].Cmd, " ")
	if !strings.Contains(launchScript, "nohup setsid 'xterm' '-e' 'echo hi'") {
		t.Errorf("launch script wrong: %s", launchScript)
	}
	if !strings.Contains(strings.Join(ex.calls[1].Cmd, " "), "scrot") {
		t.Errorf("second call should be scrot: %v", ex.calls[1].Cmd)
	}
	for _, c := range ex.calls {
		if len(c.Env) == 0 || !strings.HasPrefix(c.Env[0], "DISPLAY=") {
			t.Errorf("missing DISPLAY env in call: %v", c)
		}
	}

	var out struct {
		ArtifactKey string `json:"artifact_key"`
		Size        int    `json:"size"`
		Command     string `json:"command"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.Command != "xterm" || out.Size != len(pngBytes) {
		t.Errorf("output = %+v", out)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].ContentType != "image/png" {
		t.Errorf("artifacts = %+v", res.Artifacts)
	}
}

func TestDesktopRunAppEmptyCommand(t *testing.T) {
	eng := newDesktopEngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), DesktopRunAppAndScreenshot(),
		json.RawMessage(`{"command":""}`))
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestDesktopRunAppRequestsDesktopMode(t *testing.T) {
	p := DesktopRunAppAndScreenshot()
	if !p.NeedsSession {
		t.Error("pack should require a session")
	}
	if p.SessionSpec.Env["HELMDECK_MODE"] != "desktop" {
		t.Errorf("pack must request desktop mode via SessionSpec.Env, got %v", p.SessionSpec.Env)
	}
}

func TestDesktopRunAppScrotFailure(t *testing.T) {
	ex := &recordingExecutor{
		replies: []session.ExecResult{
			{}, // launch ok
			{ExitCode: 1, Stderr: []byte("Can't open display")},
		},
	}
	eng := newDesktopEngine(t, ex)
	_, err := eng.Execute(context.Background(), DesktopRunAppAndScreenshot(),
		json.RawMessage(`{"command":"xterm","wait_ms":1}`))
	if err == nil {
		t.Fatal("expected scrot failure to surface")
	}
	if !strings.Contains(err.Error(), "scrot exit 1") {
		t.Errorf("error should mention scrot exit: %v", err)
	}
}
