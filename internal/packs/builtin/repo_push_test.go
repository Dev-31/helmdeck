package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
)

// pushExecutorScript builds a recordingExecutor reply script with
// the four canned responses repo.push expects in the happy path:
//
//	1. git remote get-url origin    → returns the remote URL
//	2. git rev-parse --abbrev-ref   → returns the current branch
//	3. sh -c <push script>          → returns the pushed commit
//
// Tests that override one of those steps swap in their own reply.
func pushExecutorScript(remoteURL, branch, commit string, exitCodes ...int) *recordingExecutor {
	codes := make([]int, 4)
	copy(codes, exitCodes)
	return &recordingExecutor{
		replies: []session.ExecResult{
			{Stdout: []byte(remoteURL + "\n"), ExitCode: codes[0]},
			{Stdout: []byte(branch + "\n"), ExitCode: codes[1]},
			{Stdout: []byte(commit + "\n"), ExitCode: codes[2]},
		},
	}
}

func TestRepoPush_HappyPath(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com",
		[]byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----"))
	ex := pushExecutorScript("git@github.com:tosin2013/helmdeck.git", "main", "deadbeef")
	eng := newRepoEngine(t, ex)

	res, err := eng.Execute(context.Background(), RepoPush(v, nil),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-X1","remote":"origin"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ex.calls) != 3 {
		t.Fatalf("expected 3 exec calls (get-url, rev-parse, push), got %d", len(ex.calls))
	}
	// Push call must carry the SSH key on stdin and use sh -c.
	pushCall := ex.calls[2]
	if !strings.Contains(string(pushCall.Stdin), "BEGIN OPENSSH PRIVATE KEY") {
		t.Errorf("ssh key not piped to push: %q", pushCall.Stdin)
	}
	script := strings.Join(pushCall.Cmd, " ")
	if !strings.Contains(script, "git -C '/tmp/helmdeck-clone-X1' push") {
		t.Errorf("push command not in script: %s", script)
	}
	if !strings.Contains(script, "GIT_SSH_COMMAND") {
		t.Errorf("GIT_SSH_COMMAND missing: %s", script)
	}

	var out struct {
		URL        string `json:"url"`
		Remote     string `json:"remote"`
		Branch     string `json:"branch"`
		Commit     string `json:"commit"`
		Credential string `json:"credential"`
		Forced     bool   `json:"forced"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.URL != "git@github.com:tosin2013/helmdeck.git" || out.Branch != "main" || out.Commit != "deadbeef" {
		t.Errorf("output wrong: %+v", out)
	}
	if out.Credential != "deploy-key" {
		t.Errorf("credential not echoed: %s", out.Credential)
	}
	if out.Forced {
		t.Error("forced should default to false")
	}
}

func TestRepoPush_DefaultRemoteOrigin(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	ex := pushExecutorScript("git@github.com:foo/bar.git", "main", "abc")
	eng := newRepoEngine(t, ex)
	_, err := eng.Execute(context.Background(), RepoPush(v, nil),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-X1"}`))
	if err != nil {
		t.Fatal(err)
	}
	// First call should be `git remote get-url origin`.
	first := ex.calls[0].Cmd
	if first[len(first)-1] != "origin" {
		t.Errorf("default remote should be origin, got %v", first)
	}
}

func TestRepoPush_ExplicitBranchSkipsDetection(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	// When branch is supplied, the rev-parse step is skipped — only
	// 2 exec calls happen instead of 3.
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("git@github.com:foo/bar.git\n")},
		{Stdout: []byte("commitsha\n")},
	}}
	eng := newRepoEngine(t, ex)
	_, err := eng.Execute(context.Background(), RepoPush(v, nil),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-X1","branch":"feature/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.calls) != 2 {
		t.Errorf("expected 2 calls (get-url + push), got %d", len(ex.calls))
	}
	pushScript := strings.Join(ex.calls[1].Cmd, " ")
	if !strings.Contains(pushScript, "'feature/x'") {
		t.Errorf("explicit branch missing from push: %s", pushScript)
	}
}

func TestRepoPush_ForceWithLease(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	ex := pushExecutorScript("git@github.com:foo/bar.git", "main", "abc")
	eng := newRepoEngine(t, ex)
	_, err := eng.Execute(context.Background(), RepoPush(v, nil),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-X1","force":true}`))
	if err != nil {
		t.Fatal(err)
	}
	pushScript := strings.Join(ex.calls[2].Cmd, " ")
	if !strings.Contains(pushScript, "--force-with-lease") {
		t.Errorf("force should map to --force-with-lease, not raw --force: %s", pushScript)
	}
}

func TestRepoPush_NonFastForwardMapsToSchemaMismatch(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("git@github.com:foo/bar.git\n")},
		{Stdout: []byte("main\n")},
		{
			ExitCode: 1,
			Stderr: []byte(" ! [rejected]        main -> main (non-fast-forward)\n" +
				"error: failed to push some refs to 'github.com:foo/bar.git'\n" +
				"hint: Updates were rejected because the tip of your current branch is behind\n" +
				"hint: its remote counterpart. Integrate the remote changes (e.g.\n" +
				"hint: 'git pull ...') before pushing again.\n"),
		},
	}}
	eng := newRepoEngine(t, ex)
	_, err := eng.Execute(context.Background(), RepoPush(v, nil),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-X1"}`))
	if err == nil {
		t.Fatal("expected non-fast-forward to fail")
	}
	pe, ok := err.(*packs.PackError)
	if !ok {
		t.Fatalf("expected *PackError, got %T: %v", err, err)
	}
	if pe.Code != packs.CodeSchemaMismatch {
		t.Errorf("expected schema_mismatch, got %s", pe.Code)
	}
	if !strings.Contains(pe.Message, "non-fast-forward") {
		t.Errorf("error should mention non-fast-forward: %s", pe.Message)
	}
}

func TestRepoPush_OtherFailureMapsToHandlerFailed(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("git@github.com:nope/nope.git\n")},
		{Stdout: []byte("main\n")},
		{ExitCode: 128, Stderr: []byte("ERROR: Repository not found.")},
	}}
	eng := newRepoEngine(t, ex)
	_, err := eng.Execute(context.Background(), RepoPush(v, nil),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-X1"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	pe, _ := err.(*packs.PackError)
	if pe == nil || pe.Code != packs.CodeHandlerFailed {
		t.Errorf("expected handler_failed, got %v", err)
	}
}

func TestRepoPush_DetachedHeadRejected(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("git@github.com:foo/bar.git\n")},
		{Stdout: []byte("HEAD\n")}, // detached
	}}
	eng := newRepoEngine(t, ex)
	_, err := eng.Execute(context.Background(), RepoPush(v, nil),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-X1"}`))
	if err == nil {
		t.Fatal("expected detached HEAD to fail without explicit branch")
	}
	if !strings.Contains(err.Error(), "detached HEAD") {
		t.Errorf("error should mention detached HEAD: %v", err)
	}
}

func TestRepoPush_RejectsUnsafeClonePath(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	eng := newRepoEngine(t, &recordingExecutor{})
	cases := []string{
		"relative/path",
		"/etc/passwd",
		"/var/lib/garage",
		"/tmp/../etc/passwd",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			body := `{"clone_path":"` + p + `"}`
			_, err := eng.Execute(context.Background(), RepoPush(v, nil), json.RawMessage(body))
			if err == nil {
				t.Errorf("expected %q to be rejected", p)
			}
		})
	}
}

func TestRepoPush_RejectsHTTPSRemote(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("https://github.com/foo/bar.git\n")},
	}}
	eng := newRepoEngine(t, ex)
	_, err := eng.Execute(context.Background(), RepoPush(v, nil),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-X1"}`))
	if err == nil {
		t.Fatal("expected https remote to be rejected in v1")
	}
	if !strings.Contains(err.Error(), "only ssh remotes supported") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestRepoPush_RemoteHasNoURL(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("\n")}, // empty
	}}
	eng := newRepoEngine(t, ex)
	_, err := eng.Execute(context.Background(), RepoPush(v, nil),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-X1","remote":"upstream"}`))
	if err == nil {
		t.Fatal("expected error for empty remote URL")
	}
	if !strings.Contains(err.Error(), `remote "upstream" has no url`) {
		t.Errorf("wrong error: %v", err)
	}
}

func TestRepoPush_RequiresClonePath(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	eng := newRepoEngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), RepoPush(v, nil), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for missing clone_path")
	}
}

func TestIsNonFastForward(t *testing.T) {
	cases := []struct {
		stderr string
		want   bool
	}{
		{"non-fast-forward", true},
		{" ! [rejected] main -> main (non-fast-forward)", true},
		{"hint: Updates were rejected because the remote contains work that you do not have", true},
		{"fetch first", true},
		{"Permission denied (publickey).", false},
		{"fatal: repository not found", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isNonFastForward(tc.stderr)
		if got != tc.want {
			t.Errorf("isNonFastForward(%q) = %v, want %v", tc.stderr, got, tc.want)
		}
	}
}

func TestIsSafeClonePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/helmdeck-clone-X1", true},
		{"/home/helmdeck/work/repo", true},
		{"/etc/passwd", false},
		{"/var/lib/garage", false},
		{"relative/path", false},
		{"", false},
		{"/tmp/../etc/passwd", false},
	}
	for _, tc := range cases {
		got := isSafeClonePath(tc.path)
		if got != tc.want {
			t.Errorf("isSafeClonePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
