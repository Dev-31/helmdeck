package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
)

// TestRepoMap_InputValidation covers the fast-fail paths: missing
// clone_path, unsafe include glob, malformed JSON.
func TestRepoMap_InputValidation(t *testing.T) {
	eng := packs.New(
		packs.WithRuntime(fakeRuntime{}),
		packs.WithSessionExecutor(&recordingExecutor{}),
	)

	cases := []struct {
		name  string
		input string
		want  string
	}{
		// Schema-level required-field check (engine rejects before the
		// handler runs).
		{"missing clone_path", `{}`, `missing required field "clone_path"`},
		// Handler-level guards that only fire once the schema passed.
		{"relative clone_path", `{"clone_path":"relative/bad"}`, "clone_path must be an absolute path"},
		{"unsafe glob with semicolon", `{"clone_path":"/tmp/helmdeck-x","include_globs":["*.go;rm -rf /"]}`, "unsafe include glob"},
		{"unsafe glob with backtick", `{"clone_path":"/tmp/helmdeck-x","include_globs":["` + "`whoami`" + `"]}`, "unsafe include glob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.Execute(context.Background(), RepoMap(), json.RawMessage(tc.input))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestRepoMap_PassesThroughSidecarJSON — when the shell pipeline
// returns well-formed JSON on stdout, the handler surfaces it as-is.
func TestRepoMap_PassesThroughSidecarJSON(t *testing.T) {
	canned := `{"map":"main.go:\n  function main\n","tokens_estimated":4,"files_covered":1,"files_total":1}`
	ex := &recordingExecutor{replies: []session.ExecResult{{Stdout: []byte(canned)}}}
	eng := packs.New(packs.WithRuntime(fakeRuntime{}), packs.WithSessionExecutor(ex))

	res, err := eng.Execute(context.Background(), RepoMap(),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-abc","token_budget":100}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out["files_covered"].(float64) != 1 {
		t.Errorf("files_covered = %v", out["files_covered"])
	}
	if !strings.Contains(out["map"].(string), "function main") {
		t.Errorf("map content not flowed through: %v", out["map"])
	}
	// And the generated script must include the token budget we passed.
	if len(ex.calls) != 1 {
		t.Fatalf("expected 1 exec, got %d", len(ex.calls))
	}
	script := strings.Join(ex.calls[0].Cmd, " ")
	if !strings.Contains(script, "TOKEN_BUDGET=100") {
		t.Errorf("token budget not threaded into script: %s", script)
	}
}

// TestRepoMap_ClassifiesMissingCtags — sidecar emits the "ctags-missing"
// sentinel on stderr and a non-zero exit code; handler must translate
// that into a user-actionable install hint, not a generic exec failure.
func TestRepoMap_ClassifiesMissingCtags(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{
		{ExitCode: 3, Stderr: []byte("ctags-missing\n")},
	}}
	eng := packs.New(packs.WithRuntime(fakeRuntime{}), packs.WithSessionExecutor(ex))
	_, err := eng.Execute(context.Background(), RepoMap(),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-abc"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "universal-ctags is required") {
		t.Errorf("handler should surface install hint, got: %v", err)
	}
}

// TestRepoMap_ClassifiesMissingPython — same pattern for the python3
// sentinel so operators get a pointed message instead of a raw exit.
func TestRepoMap_ClassifiesMissingPython(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{
		{ExitCode: 2, Stderr: []byte("python3-missing\n")},
	}}
	eng := packs.New(packs.WithRuntime(fakeRuntime{}), packs.WithSessionExecutor(ex))
	_, err := eng.Execute(context.Background(), RepoMap(),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-abc"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "python3 is required") {
		t.Errorf("handler should surface python install hint, got: %v", err)
	}
}

// TestRepoMap_RejectsNonJSONStdout — defensive check: if the sidecar
// script ever prints something other than JSON, we must refuse to
// surface garbage to the caller.
func TestRepoMap_RejectsNonJSONStdout(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("not json here")},
	}}
	eng := packs.New(packs.WithRuntime(fakeRuntime{}), packs.WithSessionExecutor(ex))
	_, err := eng.Execute(context.Background(), RepoMap(),
		json.RawMessage(`{"clone_path":"/tmp/helmdeck-clone-abc"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "non-JSON") {
		t.Errorf("error should call out non-JSON: %v", err)
	}
}

// TestIsSafeCtagsGlob exercises the glob-injection guard table.
func TestIsSafeCtagsGlob(t *testing.T) {
	cases := []struct {
		g    string
		safe bool
	}{
		{"*.go", true},
		{"src/*.py", true},
		{"[Mm]ain.*", true},
		{"**/foo-bar_baz.ts", true},
		{"", false},
		{"*.go; rm -rf /", false},
		{"`whoami`", false},
		{"$(id)", false},
		{"foo|bar", false},
		{"foo'bar", false},
		{strings.Repeat("a", 65), false},
	}
	for _, tc := range cases {
		if got := isSafeCtagsGlob(tc.g); got != tc.safe {
			t.Errorf("isSafeCtagsGlob(%q) = %v, want %v", tc.g, got, tc.safe)
		}
	}
}

// TestRepoMap_Integration_Ranking — drive the full shell pipeline
// against a fixture repo and assert the ranker picks the file with
// the most symbols first. Requires ctags + python3 + git on PATH.
func TestRepoMap_Integration_Ranking(t *testing.T) {
	if _, err := exec.LookPath("ctags"); err != nil {
		t.Skip("ctags not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	clone := t.TempDir()
	// "core" file: five functions. "helper" file: one function.
	// Ranker should surface core.go first even though lexical order
	// would put helper.go earlier.
	mustWrite(t, filepath.Join(clone, "core.go"), `package main
func A() {}
func B() {}
func C() {}
func D() {}
func E() {}
`)
	mustWrite(t, filepath.Join(clone, "helper.go"), `package main
func Z() {}
`)
	runInDir(t, clone, "git", "init", "-q")
	runInDir(t, clone, "git", "config", "user.email", "t@e.com")
	runInDir(t, clone, "git", "config", "user.name", "T")
	runInDir(t, clone, "git", "add", ".")
	runInDir(t, clone, "git", "commit", "-q", "-m", "init")

	script := buildRepoMapScript(clone, 2000, nil)
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}

	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("non-JSON envelope: %v (raw: %s)", err, out)
	}
	mapText, _ := env["map"].(string)
	coreIdx := strings.Index(mapText, "core.go:")
	helperIdx := strings.Index(mapText, "helper.go:")
	if coreIdx < 0 || helperIdx < 0 {
		t.Fatalf("both files should appear in map, got: %s", mapText)
	}
	if coreIdx > helperIdx {
		t.Errorf("core.go (5 symbols) should outrank helper.go (1 symbol); map:\n%s", mapText)
	}
	if env["files_covered"].(float64) != 2 {
		t.Errorf("files_covered = %v, want 2", env["files_covered"])
	}
	if env["files_total"].(float64) != 2 {
		t.Errorf("files_total = %v, want 2", env["files_total"])
	}
}

// TestRepoMap_Integration_BudgetEnforced — when the token budget is
// tight, the reducer truncates the map. Assert files_covered < files_total.
func TestRepoMap_Integration_BudgetEnforced(t *testing.T) {
	if _, err := exec.LookPath("ctags"); err != nil {
		t.Skip("ctags not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	clone := t.TempDir()
	// Seven files, each with one function — forces ranking ties and
	// guarantees the budget is the limiter, not the content.
	for _, letter := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		body := "package main\nfunc " + strings.ToUpper(letter) + "() {}\n"
		mustWrite(t, filepath.Join(clone, letter+".go"), body)
	}
	runInDir(t, clone, "git", "init", "-q")
	runInDir(t, clone, "git", "config", "user.email", "t@e.com")
	runInDir(t, clone, "git", "config", "user.name", "T")
	runInDir(t, clone, "git", "add", ".")
	runInDir(t, clone, "git", "commit", "-q", "-m", "init")

	// Very tight budget: each file block is ~20 chars; at 4 chars/token
	// that's ~5 tokens. 10 total tokens allows at most 2 files.
	script := buildRepoMapScript(clone, 10, nil)
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	var env map[string]any
	_ = json.Unmarshal(out, &env)
	covered := int(env["files_covered"].(float64))
	total := int(env["files_total"].(float64))
	if total != 7 {
		t.Errorf("files_total = %d, want 7", total)
	}
	if covered >= total {
		t.Errorf("budget should have truncated: covered=%d total=%d", covered, total)
	}
	if covered < 1 {
		t.Errorf("at least one file should fit: covered=%d", covered)
	}
}

// TestRepoMap_Integration_NoSymbols — when ctags finds no parseable
// language files (e.g. a repo with only LICENSE/CHANGELOG/binary blobs),
// the reducer still emits a well-formed envelope with files_covered=0
// so the agent can branch cleanly instead of handling an error path.
// (Note: ctags parses Markdown/AsciiDoc headings as symbols, so we
// use a plain-text fixture here to get a true "no symbols" case.)
func TestRepoMap_Integration_NoSymbols(t *testing.T) {
	if _, err := exec.LookPath("ctags"); err != nil {
		t.Skip("ctags not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	clone := t.TempDir()
	mustWrite(t, filepath.Join(clone, "LICENSE"), "MIT\n")
	mustWrite(t, filepath.Join(clone, "CHANGELOG"), "initial release\n")
	runInDir(t, clone, "git", "init", "-q")
	runInDir(t, clone, "git", "config", "user.email", "t@e.com")
	runInDir(t, clone, "git", "config", "user.name", "T")
	runInDir(t, clone, "git", "add", ".")
	runInDir(t, clone, "git", "commit", "-q", "-m", "init")

	script := buildRepoMapScript(clone, 1000, nil)
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("non-JSON: %v", err)
	}
	if env["files_covered"].(float64) != 0 {
		t.Errorf("plain-text repo should have 0 files_covered, got %v", env["files_covered"])
	}
	if env["map"] != "" {
		t.Errorf("map should be empty string, got %q", env["map"])
	}
	if env["files_total"].(float64) != 2 {
		t.Errorf("files_total should reflect git ls-files count, got %v", env["files_total"])
	}
}

// TestRepoMap_Integration_PackageLockExcluded — in the real world,
// ctags parses package-lock.json into hundreds of "symbols" (one per
// JSON key) and drowns real code in the ranking. Assert the default
// exclude list keeps the lockfile out even when it's the biggest
// "symbol-bearing" file in the repo.
func TestRepoMap_Integration_PackageLockExcluded(t *testing.T) {
	if _, err := exec.LookPath("ctags"); err != nil {
		t.Skip("ctags not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	clone := t.TempDir()
	// One real Go file, one package-lock.json with many keys. Without
	// the default-exclude, the lockfile's dozens of JSON keys would
	// swamp main.go in the ranking.
	mustWrite(t, filepath.Join(clone, "main.go"), "package main\nfunc Run() {}\n")
	lockBody := `{"name":"x","version":"1","lockfileVersion":1,"dependencies":{` +
		`"a":{"version":"1"},"b":{"version":"2"},"c":{"version":"3"},` +
		`"d":{"version":"4"},"e":{"version":"5"},"f":{"version":"6"}}}`
	mustWrite(t, filepath.Join(clone, "package-lock.json"), lockBody)
	runInDir(t, clone, "git", "init", "-q")
	runInDir(t, clone, "git", "config", "user.email", "t@e.com")
	runInDir(t, clone, "git", "config", "user.name", "T")
	runInDir(t, clone, "git", "add", ".")
	runInDir(t, clone, "git", "commit", "-q", "-m", "init")

	script := buildRepoMapScript(clone, 2000, nil)
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	var env map[string]any
	_ = json.Unmarshal(out, &env)
	m, _ := env["map"].(string)
	if strings.Contains(m, "package-lock.json") {
		t.Errorf("package-lock.json should be excluded from ranking, but appeared in map:\n%s", m)
	}
	if !strings.Contains(m, "main.go") {
		t.Errorf("main.go should be in the map; got:\n%s", m)
	}
}

// TestRepoMap_Integration_LanguagesFilter — `languages: ["Go"]` at
// the input should cause ctags to skip non-Go files entirely. We
// seed a Go file + a Python file and assert only Go surfaces.
func TestRepoMap_Integration_LanguagesFilter(t *testing.T) {
	if _, err := exec.LookPath("ctags"); err != nil {
		t.Skip("ctags not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	clone := t.TempDir()
	mustWrite(t, filepath.Join(clone, "a.go"), "package main\nfunc GoFunc() {}\n")
	mustWrite(t, filepath.Join(clone, "b.py"), "def py_func():\n    pass\n")
	runInDir(t, clone, "git", "init", "-q")
	runInDir(t, clone, "git", "config", "user.email", "t@e.com")
	runInDir(t, clone, "git", "config", "user.name", "T")
	runInDir(t, clone, "git", "add", ".")
	runInDir(t, clone, "git", "commit", "-q", "-m", "init")

	script := buildRepoMapScript(clone, 2000, []string{"Go"})
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	var env map[string]any
	_ = json.Unmarshal(out, &env)
	m, _ := env["map"].(string)
	if !strings.Contains(m, "a.go") {
		t.Errorf("a.go should be in map with --languages=Go; got:\n%s", m)
	}
	if strings.Contains(m, "b.py") {
		t.Errorf("b.py should be filtered out by --languages=Go; got:\n%s", m)
	}
}

// TestIsSafeCtagsLanguage exercises the language-name validator.
func TestIsSafeCtagsLanguage(t *testing.T) {
	cases := []struct {
		s    string
		safe bool
	}{
		{"Go", true},
		{"Python", true},
		{"JavaScript", true},
		{"C++", true},
		{"C#", true},
		{"F#", true},
		{"Objective-C", true},
		{"", false},
		{"Go; rm -rf /", false},
		{"$(id)", false},
		{strings.Repeat("a", 33), false},
	}
	for _, tc := range cases {
		if got := isSafeCtagsLanguage(tc.s); got != tc.safe {
			t.Errorf("isSafeCtagsLanguage(%q) = %v, want %v", tc.s, got, tc.safe)
		}
	}
}

// Helper: ensure the test setup is sane by running ctags directly
// against a fixture and asserting it emits JSON. If this fails, the
// integration tests above won't be meaningful.
func TestCtagsSanity(t *testing.T) {
	if _, err := exec.LookPath("ctags"); err != nil {
		t.Skip("ctags not available")
	}
	clone := t.TempDir()
	mustWrite(t, filepath.Join(clone, "main.go"), "package main\nfunc Hello() {}\n")
	out, err := exec.Command("ctags", "-R", "--output-format=json", "--fields=+K+n", clone).Output()
	if err != nil {
		t.Fatalf("ctags: %v", err)
	}
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var tag map[string]any
		if json.Unmarshal([]byte(line), &tag) == nil && tag["name"] == "Hello" {
			found = true
		}
	}
	if !found {
		t.Errorf("ctags JSON output did not contain Hello symbol: %s", out)
	}
	// Reference os/filepath to keep the imports used by the fixture
	// layout; some builds strip imports otherwise.
	_ = filepath.Join
	_ = os.Getenv
}
