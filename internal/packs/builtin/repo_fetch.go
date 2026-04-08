package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/security"
	"github.com/tosin2013/helmdeck/internal/session"
	"github.com/tosin2013/helmdeck/internal/vault"
)

// RepoFetch (T505, ADR 022) clones a git repository into the session
// container using a vault-resolved SSH key. The agent never sees the
// key — the pack writes it to a temporary file inside the session,
// runs git via GIT_SSH_COMMAND with `accept-new` host-key policy,
// then deletes the key file before returning.
//
// Input shape:
//
//	{
//	  "url":     "git@github.com:tosin2013/helmdeck.git",  // required
//	  "ref":     "main",                                    // optional, default HEAD
//	  "depth":   1                                          // optional, shallow clone
//	}
//
// Output shape:
//
//	{
//	  "url":         "git@github.com:tosin2013/helmdeck.git",
//	  "ref":         "main",
//	  "commit":      "abc1234...",
//	  "credential":  "github-deploy-key",
//	  "files":       42,
//	  "clone_path":  "/tmp/helmdeck-clone-<rand>"
//	}
//
// The clone is left on the session container's filesystem so
// follow-on packs (repo.push, slides.video assembling assets) can
// read it. The clone path is returned in the output so callers can
// reference it. The session lifetime is the natural cleanup
// boundary — when the session terminates, the clone goes with it.
//
// URL forms accepted:
//
//	git@github.com:owner/repo.git           — SSH (canonical)
//	ssh://git@github.com/owner/repo.git     — SSH (URL form)
//	https://github.com/owner/repo.git       — HTTPS, vault credential
//	                                           must be type=api_key
//	                                           (used as a Bearer token
//	                                           via the GIT_ASKPASS path)
//
// For the v1 of this pack we only implement the SSH path. HTTPS
// support lands in a follow-up alongside the placeholder-token
// gateway (T504).
func RepoFetch(v *vault.Store, eg *security.EgressGuard) *packs.Pack {
	return &packs.Pack{
		Name:        "repo.fetch",
		Version:     "v1",
		Description: "Clone a git repository inside the session container using a vault-resolved SSH key.",
		NeedsSession: true,
		InputSchema: packs.BasicSchema{
			Required: []string{"url"},
			Properties: map[string]string{
				"url":   "string",
				"ref":   "string",
				"depth": "number",
			},
		},
		OutputSchema: packs.BasicSchema{
			Required: []string{"url", "commit", "clone_path"},
			Properties: map[string]string{
				"url":        "string",
				"ref":        "string",
				"commit":     "string",
				"credential": "string",
				"files":      "number",
				"clone_path": "string",
			},
		},
		Handler: repoFetchHandler(v, eg),
	}
}

type repoFetchInput struct {
	URL   string `json:"url"`
	Ref   string `json:"ref"`
	Depth int    `json:"depth"`
}

func repoFetchHandler(v *vault.Store, eg *security.EgressGuard) packs.HandlerFunc {
	return func(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
		var in repoFetchInput
		if err := json.Unmarshal(ec.Input, &in); err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
		}
		if strings.TrimSpace(in.URL) == "" {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "url is required"}
		}
		if v == nil {
			return nil, &packs.PackError{Code: packs.CodeSessionUnavailable, Message: "credential vault not configured"}
		}
		if ec.Exec == nil {
			return nil, &packs.PackError{Code: packs.CodeSessionUnavailable, Message: "engine has no session executor"}
		}

		host, scheme, err := parseGitHost(in.URL)
		if err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
		}
		if scheme != "ssh" {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput,
				Message: fmt.Sprintf("only ssh URLs supported in v1; got %q (https support lands with T504)", scheme)}
		}

		// T508: SSRF / metadata-IP guard. The egress guard refuses
		// any host that resolves to a private, loopback, link-local,
		// or cloud-metadata range — even via DNS rebinding tricks.
		// nil guard = guard disabled (dev/test mode); production
		// deployments always wire one in via cmd/control-plane.
		if eg != nil {
			if err := eg.CheckHost(ctx, host); err != nil {
				return nil, &packs.PackError{Code: packs.CodeInvalidInput,
					Message: fmt.Sprintf("egress denied: %v", err), Cause: err}
			}
		}

		// Resolve an SSH credential for the host. Actor identity comes
		// from the engine's audit context — the engine sets it when
		// the pack is invoked via the REST endpoint. For now we use
		// a wildcard subject so dev-mode (no auth) calls work; the
		// REST layer will tighten this in T501c follow-on by passing
		// the JWT actor through to the engine.
		actor := vault.Actor{Subject: "*"}
		res, err := v.Resolve(ctx, actor, host, "")
		if err != nil {
			if errors.Is(err, vault.ErrNoMatch) {
				return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
					Message: fmt.Sprintf("no vault credential matches host %q", host)}
			}
			if errors.Is(err, vault.ErrDenied) {
				return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
					Message: fmt.Sprintf("vault denied access to credential for host %q", host)}
			}
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: err.Error(), Cause: err}
		}
		if res.Record.Type != vault.TypeSSH {
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
				Message: fmt.Sprintf("vault credential %q is type %q, expected ssh", res.Record.Name, res.Record.Type)}
		}

		ref := in.Ref
		depth := in.Depth
		if depth < 0 {
			depth = 0
		}

		// Run a single shell script that:
		//  1. mktemp -d for the clone destination AND a private key
		//     file (chmod 600).
		//  2. write the SSH key from stdin into the key file.
		//  3. git clone with GIT_SSH_COMMAND set to use that key with
		//     accept-new host-key policy (no prompt, but learn the
		//     fingerprint on first use).
		//  4. echo a JSON envelope with clone path + commit + file
		//     count to stdout.
		//  5. shred the key file before returning.
		//
		// All five steps are wrapped in `set -eu` so any failure
		// surfaces as a non-zero exit and the rm runs in a trap so
		// the key never persists past the script even on error.
		script := buildRepoFetchScript(in.URL, ref, depth)
		execRes, err := ec.Exec(ctx, session.ExecRequest{
			Cmd:   []string{"sh", "-c", script},
			Stdin: res.Plaintext,
		})
		if err != nil {
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
				Message: fmt.Sprintf("git clone exec: %v", err)}
		}
		if execRes.ExitCode != 0 {
			stderr := string(execRes.Stderr)
			if len(stderr) > 1024 {
				stderr = stderr[:1024] + "...(truncated)"
			}
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
				Message: fmt.Sprintf("git clone exit %d: %s", execRes.ExitCode, stderr)}
		}

		// Parse the JSON envelope the script writes to stdout.
		// Anything else is treated as a script bug.
		var envelope struct {
			ClonePath string `json:"clone_path"`
			Commit    string `json:"commit"`
			Files     int    `json:"files"`
		}
		if err := json.Unmarshal(execRes.Stdout, &envelope); err != nil {
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
				Message: fmt.Sprintf("could not parse clone envelope: %v (raw: %q)", err, truncateString(string(execRes.Stdout), 256))}
		}

		return json.Marshal(map[string]any{
			"url":        in.URL,
			"ref":        ref,
			"commit":     envelope.Commit,
			"credential": res.Record.Name,
			"files":      envelope.Files,
			"clone_path": envelope.ClonePath,
		})
	}
}

// parseGitHost extracts the host portion of a git URL and identifies
// the transport scheme. Supports the three forms documented on the
// pack: scp-like (git@host:owner/repo), ssh:// URL, and https:// URL.
func parseGitHost(rawURL string) (host, scheme string, err error) {
	// scp-like form: user@host:path. The colon distinguishes it from
	// a normal URL because the part after `user@` doesn't have //.
	if !strings.Contains(rawURL, "://") {
		at := strings.Index(rawURL, "@")
		colon := strings.Index(rawURL, ":")
		if at < 0 || colon < at {
			return "", "", fmt.Errorf("malformed git url: %s", rawURL)
		}
		return rawURL[at+1 : colon], "ssh", nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	if u.Hostname() == "" {
		return "", "", fmt.Errorf("missing host in url: %s", rawURL)
	}
	switch u.Scheme {
	case "ssh", "git+ssh":
		return u.Hostname(), "ssh", nil
	case "https", "http":
		return u.Hostname(), "https", nil
	default:
		return "", "", fmt.Errorf("unsupported git scheme: %s", u.Scheme)
	}
}

// buildRepoFetchScript renders the shell pipeline that clones a repo
// using a key passed on stdin. Built as a string here so the test can
// assert on its shape; production callers run it via session.Exec.
//
// The script:
//  1. mktemp -d for the key dir AND the clone dir (both 0700).
//  2. Write the SSH key from stdin into the key file (0600).
//  3. trap EXIT to shred the key on every exit path.
//  4. git clone with GIT_SSH_COMMAND pointing at the key file.
//  5. Optional `git checkout <ref>` if ref was supplied.
//  6. Capture commit hash and file count.
//  7. Print JSON envelope to stdout.
//
// Stderr is reserved for git's progress / error output, which the
// pack handler surfaces in the PackError message on failure.
func buildRepoFetchScript(url, ref string, depth int) string {
	depthFlag := ""
	if depth > 0 {
		depthFlag = fmt.Sprintf("--depth %d ", depth)
	}
	lines := []string{
		"set -eu",
		"KEY_DIR=$(mktemp -d /tmp/helmdeck-key-XXXXXX)",
		"CLONE_DIR=$(mktemp -d /tmp/helmdeck-clone-XXXXXX)",
		"trap 'shred -u \"$KEY_DIR\"/id_rsa 2>/dev/null || rm -f \"$KEY_DIR\"/id_rsa; rmdir \"$KEY_DIR\" 2>/dev/null || true' EXIT",
		"cat > \"$KEY_DIR\"/id_rsa",
		"chmod 600 \"$KEY_DIR\"/id_rsa",
		"export GIT_SSH_COMMAND=\"ssh -i $KEY_DIR/id_rsa -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$KEY_DIR/known_hosts -o IdentitiesOnly=yes\"",
		"git clone " + depthFlag + shellQuote(url) + " \"$CLONE_DIR\" 1>&2",
	}
	if ref != "" {
		lines = append(lines, "git -C \"$CLONE_DIR\" checkout "+shellQuote(ref)+" 1>&2")
	}
	lines = append(lines,
		"COMMIT=$(git -C \"$CLONE_DIR\" rev-parse HEAD)",
		"FILES=$(git -C \"$CLONE_DIR\" ls-files | wc -l | tr -d ' ')",
		"printf '{\"clone_path\":\"%s\",\"commit\":\"%s\",\"files\":%s}' \"$CLONE_DIR\" \"$COMMIT\" \"$FILES\"",
	)
	return strings.Join(lines, "\n")
}
