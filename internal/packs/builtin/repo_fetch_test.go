package builtin

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/security"
	"github.com/tosin2013/helmdeck/internal/session"
	"github.com/tosin2013/helmdeck/internal/store"
	"github.com/tosin2013/helmdeck/internal/vault"
)

// vaultWithSSHCred constructs an in-memory vault store with one SSH
// credential matching the given host. Returns the store + the
// credential id so tests can grant + assert on it.
func vaultWithSSHCred(t *testing.T, host string, payload []byte) *vault.Store {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key := make([]byte, 32)
	v, err := vault.New(db, key)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := v.Create(context.Background(), vault.CreateInput{
		Name: "deploy-key", Type: vault.TypeSSH, HostPattern: host, Plaintext: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Grant(context.Background(), rec.ID, vault.Grant{ActorSubject: "*"}); err != nil {
		t.Fatal(err)
	}
	return v
}

func newRepoEngine(t *testing.T, ex *recordingExecutor) *packs.Engine {
	t.Helper()
	return packs.New(
		packs.WithRuntime(fakeRuntime{}),
		packs.WithSessionExecutor(ex),
	)
}

func TestRepoFetch_HappyPath(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----"))
	envelope := `{"clone_path":"/tmp/helmdeck-clone-abc","commit":"deadbeef","files":42}`
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte(envelope)},
	}}
	eng := newRepoEngine(t, ex)

	res, err := eng.Execute(context.Background(), RepoFetch(v, nil),
		json.RawMessage(`{"url":"git@github.com:tosin2013/helmdeck.git","ref":"main","depth":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(ex.calls))
	}
	// Stdin must be the SSH private key.
	if !strings.Contains(string(ex.calls[0].Stdin), "BEGIN OPENSSH PRIVATE KEY") {
		t.Errorf("ssh key not piped via stdin: %q", ex.calls[0].Stdin)
	}
	// Script must include git clone with the URL and the depth flag.
	script := strings.Join(ex.calls[0].Cmd, " ")
	if !strings.Contains(script, "git clone --depth 1") {
		t.Errorf("depth flag missing from script: %s", script)
	}
	if !strings.Contains(script, "tosin2013/helmdeck.git") {
		t.Errorf("repo URL missing from script: %s", script)
	}
	if !strings.Contains(script, "GIT_SSH_COMMAND") {
		t.Errorf("GIT_SSH_COMMAND missing: %s", script)
	}
	if !strings.Contains(script, "checkout 'main'") {
		t.Errorf("ref checkout missing: %s", script)
	}

	var out struct {
		URL        string `json:"url"`
		Commit     string `json:"commit"`
		Credential string `json:"credential"`
		Files      int    `json:"files"`
		ClonePath  string `json:"clone_path"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.Commit != "deadbeef" || out.Files != 42 || out.ClonePath != "/tmp/helmdeck-clone-abc" {
		t.Errorf("envelope not surfaced: %+v", out)
	}
	if out.Credential != "deploy-key" {
		t.Errorf("credential name not echoed: %s", out.Credential)
	}
}

func TestRepoFetch_NoVaultMatch(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	eng := newRepoEngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), RepoFetch(v, nil),
		json.RawMessage(`{"url":"git@gitlab.com:foo/bar.git"}`))
	if err == nil {
		t.Fatal("expected error for unmatched host")
	}
	if !strings.Contains(err.Error(), "no vault credential matches") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestRepoFetch_HTTPSPublicClone(t *testing.T) {
	// HTTPS without a credential should attempt a public clone.
	rec := &recordingExecutor{
		replies: []session.ExecResult{{
			Stdout: []byte(`{"clone_path":"/tmp/helmdeck-clone-abc","commit":"deadbeef","files":3}`),
		}},
	}
	eng := newRepoEngine(t, rec)
	res, err := eng.Execute(context.Background(), RepoFetch(nil, nil),
		json.RawMessage(`{"url":"https://github.com/octocat/Hello-World.git"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out["clone_path"] != "/tmp/helmdeck-clone-abc" {
		t.Errorf("clone_path = %v", out["clone_path"])
	}
	// Verify the script does NOT contain GIT_ASKPASS (no credential)
	if len(rec.calls) > 0 {
		script := strings.Join(rec.calls[0].Cmd, " ")
		if strings.Contains(script, "GIT_ASKPASS") {
			t.Error("public clone should not set GIT_ASKPASS in the command")
		}
	}
}

func TestRepoFetch_WrongCredentialType(t *testing.T) {
	db, _ := store.Open(":memory:")
	t.Cleanup(func() { db.Close() })
	key := make([]byte, 32)
	v, _ := vault.New(db, key)
	rec, _ := v.Create(context.Background(), vault.CreateInput{
		Name: "wrong", Type: vault.TypeAPIKey, HostPattern: "github.com", Plaintext: []byte("token"),
	})
	_ = v.Grant(context.Background(), rec.ID, vault.Grant{ActorSubject: "*"})

	eng := newRepoEngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), RepoFetch(v, nil),
		json.RawMessage(`{"url":"git@github.com:foo/bar.git"}`))
	if err == nil {
		t.Fatal("expected error for non-ssh credential type")
	}
	if !strings.Contains(err.Error(), "expected ssh") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestRepoFetch_GitCloneFailureBubbles(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	ex := &recordingExecutor{replies: []session.ExecResult{
		{ExitCode: 128, Stderr: []byte("fatal: repository not found")},
	}}
	eng := newRepoEngine(t, ex)
	_, err := eng.Execute(context.Background(), RepoFetch(v, nil),
		json.RawMessage(`{"url":"git@github.com:nope/nope.git"}`))
	if err == nil {
		t.Fatal("expected git clone failure to surface")
	}
	if !strings.Contains(err.Error(), "exit 128") || !strings.Contains(err.Error(), "repository not found") {
		t.Errorf("error should include git's stderr: %v", err)
	}
}

func TestRepoFetch_RequiresURL(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	eng := newRepoEngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), RepoFetch(v, nil), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for missing url")
	}
}

func TestParseGitHost(t *testing.T) {
	cases := []struct {
		url    string
		host   string
		scheme string
		err    bool
	}{
		{"git@github.com:tosin2013/helmdeck.git", "github.com", "ssh", false},
		{"ssh://git@github.com/tosin2013/helmdeck.git", "github.com", "ssh", false},
		{"https://github.com/tosin2013/helmdeck.git", "github.com", "https", false},
		{"http://gitlab.local:8080/foo/bar.git", "gitlab.local", "https", false},
		{"ftp://example.com/repo.git", "", "", true},
		{"not-a-url", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			host, scheme, err := parseGitHost(tc.url)
			if tc.err {
				if err == nil {
					t.Errorf("expected error")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if host != tc.host || scheme != tc.scheme {
				t.Errorf("got (%s, %s), want (%s, %s)", host, scheme, tc.host, tc.scheme)
			}
		})
	}
}

func TestBuildRepoFetchScript_OmitsCheckoutWithoutRef(t *testing.T) {
	script := buildRepoFetchSSHScript("git@github.com:foo/bar.git", "", 0)
	if strings.Contains(script, "checkout") {
		t.Errorf("empty ref should produce no checkout line")
	}
	if strings.Contains(script, "--depth") {
		t.Errorf("zero depth should produce no --depth flag")
	}
}

// T508 — verify the egress guard blocks the pack before any vault
// or executor work happens. Uses a stub resolver that returns the
// metadata IP for the requested host.
func TestRepoFetch_EgressGuardBlocksMetadataHost(t *testing.T) {
	v := vaultWithSSHCred(t, "evil.example", []byte("key"))
	eg := security.New(security.WithResolver(stubMetaResolver{}))
	ex := &recordingExecutor{}
	eng := newRepoEngine(t, ex)

	_, err := eng.Execute(context.Background(), RepoFetch(v, eg),
		json.RawMessage(`{"url":"git@evil.example:foo/bar.git"}`))
	if err == nil {
		t.Fatal("expected egress guard to block metadata-resolving host")
	}
	if !strings.Contains(err.Error(), "egress denied") {
		t.Errorf("wrong error: %v", err)
	}
	// The handler must short-circuit BEFORE the executor sees anything.
	if len(ex.calls) != 0 {
		t.Errorf("executor should not be invoked when egress is blocked, got %d calls", len(ex.calls))
	}
}

// stubMetaResolver returns the AWS/GCP/Azure cloud metadata IP for
// every lookup. Used to simulate the SSRF attack the egress guard
// is supposed to refuse.
type stubMetaResolver struct{}

func (stubMetaResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
}

// --- context-envelope tests (2026-04-15 revision) ------------------------

// TestRepoFetch_EnvelopePassthrough asserts the handler flows the full
// SEP-1686-style context envelope (tree/readme/entrypoints/signals) from
// the script's stdout into the pack output without dropping fields. The
// script runs inside the sidecar; here we inject a canned envelope to
// exercise the handler's merge logic.
func TestRepoFetch_EnvelopePassthrough(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	envelope := `{
      "clone_path": "/tmp/clone",
      "commit": "deadbeef",
      "files": 42,
      "tree": ["README.adoc", "Makefile", "content/01.adoc"],
      "tree_total": 42,
      "tree_truncated": false,
      "readme": {"path": "README.adoc", "content": "= Workshop\n", "truncated": false},
      "entrypoints": [{"path": "Makefile", "kind": "build"}],
      "doc_hints": ["README*"],
      "signals": {"has_readme": true, "has_docs_dir": true, "has_code": false,
                  "doc_file_count": 5, "code_file_count": 0, "sparse": false}
    }`
	ex := &recordingExecutor{replies: []session.ExecResult{{Stdout: []byte(envelope)}}}
	eng := newRepoEngine(t, ex)

	res, err := eng.Execute(context.Background(), RepoFetch(v, nil),
		json.RawMessage(`{"url":"git@github.com:foo/bar.git"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	// Handler-supplied fields merged on top.
	if out["url"] != "git@github.com:foo/bar.git" {
		t.Errorf("url not surfaced: %v", out["url"])
	}
	if out["credential"] != "deploy-key" {
		t.Errorf("credential not surfaced: %v", out["credential"])
	}
	// Script-supplied fields flowed through.
	for _, key := range []string{"tree", "tree_total", "tree_truncated", "readme", "entrypoints", "doc_hints", "signals"} {
		if _, ok := out[key]; !ok {
			t.Errorf("envelope field %q missing from output", key)
		}
	}
	readme, _ := out["readme"].(map[string]any)
	if readme["path"] != "README.adoc" {
		t.Errorf("readme.path: got %v", readme["path"])
	}
	signals, _ := out["signals"].(map[string]any)
	if signals["has_readme"] != true {
		t.Errorf("signals.has_readme should be true")
	}
}

// TestRepoFetch_LegacyEnvelopeStillWorks — if the sidecar lacks python3,
// the shell fallback emits only clone_path/commit/files. The handler
// should accept that shape without synthesising or erroring on the
// missing context fields.
func TestRepoFetch_LegacyEnvelopeStillWorks(t *testing.T) {
	v := vaultWithSSHCred(t, "github.com", []byte("key"))
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte(`{"clone_path":"/tmp/clone","commit":"cafef00d","files":3}`)},
	}}
	eng := newRepoEngine(t, ex)
	res, err := eng.Execute(context.Background(), RepoFetch(v, nil),
		json.RawMessage(`{"url":"git@github.com:foo/bar.git"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out["clone_path"] != "/tmp/clone" || out["commit"] != "cafef00d" {
		t.Errorf("legacy fields not surfaced: %+v", out)
	}
	// The envelope fields simply shouldn't appear — no crash, no synthesis.
	if _, hasTree := out["tree"]; hasTree {
		t.Errorf("legacy path should not invent tree field")
	}
}

// TestRepoFetchEnvelopeScript_ShellStructure — sanity check that the
// script embeds the python3 branch, the heredoc marker, and the busybox
// fallback. Catches copy-paste errors in the raw-string constant.
func TestRepoFetchEnvelopeScript_ShellStructure(t *testing.T) {
	for _, marker := range []string{
		"command -v python3",
		"<<'PYEOF'",
		"PYEOF",
		`"tree":`,
		`"signals":`,
		`"readme":`,
		`"entrypoints":`,
		"printf '{\"clone_path\":",
	} {
		if !strings.Contains(repoFetchEnvelopeScript, marker) {
			t.Errorf("envelope script missing marker %q", marker)
		}
	}
}

// TestRepoFetchEnvelopeScript_Integration runs the real script against
// a fixture git repo. Exercises the python3 path end-to-end: README
// auto-detect, entrypoint detection, signal computation. Skips when
// python3 or git is unavailable.
func TestRepoFetchEnvelopeScript_Integration(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	clone := filepath.Join(dir, "repo")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed a realistic docs-heavy repo (mirrors the workshop repo
	// that broke OpenClaw: README.adoc, content/, docs/, Makefile).
	mustWrite(t, filepath.Join(clone, "README.adoc"), "= Workshop\n\nLow-latency performance tutorial.\n")
	mustWrite(t, filepath.Join(clone, "Makefile"), "build:\n\techo hi\n")
	if err := os.MkdirAll(filepath.Join(clone, "content"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(clone, "content", "01-intro.adoc"), "= Intro\n")
	if err := os.MkdirAll(filepath.Join(clone, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(clone, "docs", "arch.md"), "# Architecture\n")
	// Initialize git so `git ls-files` returns the tracked set.
	runInDir(t, clone, "git", "init", "-q")
	runInDir(t, clone, "git", "config", "user.email", "test@example.com")
	runInDir(t, clone, "git", "config", "user.name", "Test")
	runInDir(t, clone, "git", "add", ".")
	runInDir(t, clone, "git", "commit", "-q", "-m", "init")

	// Invoke the script with CLONE_DIR pre-set (skipping the clone
	// step; we already have a repo on disk).
	script := "CLONE_DIR=" + shellQuote(clone) + "\n" + repoFetchEnvelopeScript
	cmd := exec.Command("sh", "-c", script)
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("script exec: %v (stderr: %s)", err, cmd.Stderr)
	}

	var env map[string]any
	if err := json.Unmarshal(stdout, &env); err != nil {
		t.Fatalf("envelope not JSON: %v (raw: %s)", err, stdout)
	}

	// README must be auto-detected despite the .adoc extension (the
	// exact failure mode that broke OpenClaw's workshop-repo attempt).
	readme, _ := env["readme"].(map[string]any)
	if readme == nil {
		t.Fatalf("readme should be present for README.adoc, got nil (env: %+v)", env)
	}
	if readme["path"] != "README.adoc" {
		t.Errorf("readme.path = %v, want README.adoc", readme["path"])
	}
	if content, _ := readme["content"].(string); !strings.Contains(content, "Workshop") {
		t.Errorf("readme.content did not capture fixture text")
	}

	// Entrypoints must surface Makefile.
	entrypoints, _ := env["entrypoints"].([]any)
	var foundMake bool
	for _, ep := range entrypoints {
		if m, _ := ep.(map[string]any); m["path"] == "Makefile" {
			foundMake = true
		}
	}
	if !foundMake {
		t.Errorf("Makefile should be detected as entrypoint")
	}

	// Signals must reflect "docs-heavy, not sparse" for this fixture.
	signals, _ := env["signals"].(map[string]any)
	if signals["has_readme"] != true {
		t.Errorf("has_readme should be true")
	}
	if signals["has_docs_dir"] != true {
		t.Errorf("has_docs_dir should be true (docs/ and content/ present)")
	}
	if signals["sparse"] != false {
		t.Errorf("sparse should be false for a 3-doc-file repo")
	}

	// Tree must include relative paths (git ls-files output).
	tree, _ := env["tree"].([]any)
	if len(tree) < 3 {
		t.Errorf("tree should have at least 3 entries, got %d", len(tree))
	}
}

// TestRepoFetchEnvelopeScript_SparseRepo asserts the `sparse` signal
// fires for a genuinely-bare repo so the agent has a deterministic
// flag to branch on when telling the user "I can't make sense of
// this."
func TestRepoFetchEnvelopeScript_SparseRepo(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	clone := filepath.Join(dir, "bare")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	// Single stub file, no README, no docs, no code.
	mustWrite(t, filepath.Join(clone, "LICENSE"), "MIT\n")
	runInDir(t, clone, "git", "init", "-q")
	runInDir(t, clone, "git", "config", "user.email", "t@e.com")
	runInDir(t, clone, "git", "config", "user.name", "T")
	runInDir(t, clone, "git", "add", ".")
	runInDir(t, clone, "git", "commit", "-q", "-m", "init")

	script := "CLONE_DIR=" + shellQuote(clone) + "\n" + repoFetchEnvelopeScript
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("script exec: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	sig, _ := env["signals"].(map[string]any)
	if sig["sparse"] != true {
		t.Errorf("sparse should be true for a single-LICENSE repo, signals=%+v", sig)
	}
	if sig["has_readme"] != false {
		t.Errorf("has_readme should be false")
	}
}

// mustWrite writes a fixture file and fatals on error.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runInDir runs a command in the given directory and fatals on error.
func runInDir(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
