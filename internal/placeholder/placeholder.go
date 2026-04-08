// Package placeholder implements the placeholder-token egress
// gateway described in T504 / ADR 007.
//
// The agent never sees real credentials. When it wants to call an
// external API it embeds a placeholder token of the form
// ${vault:NAME} in URLs, headers, or request bodies. The Resolver
// scans for those patterns, looks each NAME up in the credential
// vault (gated by the per-credential ACL), and substitutes the
// plaintext payload before the request leaves the helmdeck
// process.
//
// Two consumption patterns ship in v1:
//
//   1. Substitute(ctx, actor, s) — string-level substitution. Pack
//      handlers that build their own request URL/body call this
//      directly before constructing an http.Request.
//
//   2. HTTPClient(actor) — returns an http.Client whose
//      RoundTripper rewrites the URL, every header value, and the
//      request body in transit. Pack handlers that just want a
//      placeholder-aware http.Client (the http.fetch pack, the
//      doc.ocr source_url path) hold this client.
//
// What's deliberately NOT in v1:
//
//   - HTTPS MITM forward proxy. Routing arbitrary in-container HTTP
//     traffic through a helmdeck proxy with a bundled CA cert is a
//     much bigger ship and breaks pinned-cert clients. Pack
//     handlers that need to talk to APIs use the wrapped client
//     instead.
//
//   - Body streaming. v1 buffers the entire request body so the
//     placeholder scan can run in one pass. Bodies larger than
//     bodyScanLimit are forwarded unchanged with a logged warning.
//
//   - Output substitution. The resolver only rewrites OUTBOUND
//     traffic. Responses pass through verbatim — agents see the
//     real API response, just never the credential that authorized
//     it.
package placeholder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tosin2013/helmdeck/internal/vault"
)

// ErrUnknownPlaceholder is returned when Substitute encounters a
// ${vault:NAME} pattern referencing a credential that doesn't exist
// in the vault. Pack handlers should propagate this as
// CodeInvalidInput so the LLM gets a clear "no such credential"
// signal instead of a confusing downstream HTTP error.
var ErrUnknownPlaceholder = errors.New("placeholder: unknown vault credential")

// ErrDenied is returned when the actor has no ACL grant on a
// credential they tried to substitute. Surfaced as CodeInvalidInput
// at the pack layer.
var ErrDenied = errors.New("placeholder: actor denied access to vault credential")

// placeholderRE matches ${vault:NAME}. NAME accepts the same
// character set as vault credential names — alphanumerics, dash,
// underscore, dot. Anything else terminates the match.
var placeholderRE = regexp.MustCompile(`\$\{vault:([A-Za-z0-9_\-\.]+)\}`)

// bodyScanLimit caps how large a request body Substitute will buffer
// in memory. 4 MiB is generous for typical JSON/form payloads and
// keeps a runaway upload from exhausting helmdeck's heap.
const bodyScanLimit = 4 << 20

// Resolver is the placeholder-token substitution engine. Holds a
// vault.Store reference and is goroutine-safe.
type Resolver struct {
	vault  *vault.Store
	logger *slog.Logger
}

// New constructs a Resolver. logger may be nil — defaults to
// slog.Default().
func New(v *vault.Store, logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{vault: v, logger: logger}
}

// Substitute scans s for ${vault:NAME} patterns and replaces each
// with the corresponding credential plaintext for actor. Returns
// ErrUnknownPlaceholder if any name is missing, ErrDenied if any
// is forbidden by ACL.
//
// The returned string is the substituted form; the original is left
// unchanged. Multiple references to the same credential are
// resolved once and cached in a per-call lookup map so a body that
// uses ${vault:foo} ten times only triggers one vault read.
func (r *Resolver) Substitute(ctx context.Context, actor vault.Actor, s string) (string, error) {
	if r == nil || r.vault == nil {
		return s, nil
	}
	if !strings.Contains(s, "${vault:") {
		return s, nil
	}
	cache := make(map[string]string)
	var firstErr error
	out := placeholderRE.ReplaceAllStringFunc(s, func(match string) string {
		if firstErr != nil {
			return match
		}
		groups := placeholderRE.FindStringSubmatch(match)
		if len(groups) != 2 {
			return match
		}
		name := groups[1]
		if v, ok := cache[name]; ok {
			return v
		}
		res, err := r.vault.ResolveByName(ctx, actor, name)
		if err != nil {
			if errors.Is(err, vault.ErrNotFound) {
				firstErr = fmt.Errorf("%w: %s", ErrUnknownPlaceholder, name)
				return match
			}
			if errors.Is(err, vault.ErrDenied) {
				firstErr = fmt.Errorf("%w: %s", ErrDenied, name)
				return match
			}
			firstErr = err
			return match
		}
		val := string(res.Plaintext)
		cache[name] = val
		return val
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// HTTPClient returns an http.Client that substitutes placeholder
// tokens in every outbound request URL, every header value, and the
// request body before forwarding. Use this when a pack handler
// needs to make HTTP calls that the agent has parameterized with
// vault references.
//
// Caveats:
//
//   - Bodies larger than bodyScanLimit are forwarded unchanged
//     (logged at warn level). Substitute small JSON/form payloads.
//   - The wrapped client uses a 30-second default timeout. Pack
//     handlers that need a different timeout should construct
//     their own client and call Substitute manually.
//   - Response bodies are forwarded verbatim. The resolver only
//     rewrites OUTBOUND traffic.
func (r *Resolver) HTTPClient(actor vault.Actor) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &substitutingTransport{
			resolver: r,
			actor:    actor,
			next:     http.DefaultTransport,
		},
	}
}

// substitutingTransport is the http.RoundTripper the wrapped
// client installs. It rewrites the request in-place before
// delegating to the underlying transport.
type substitutingTransport struct {
	resolver *Resolver
	actor    vault.Actor
	next     http.RoundTripper
}

func (t *substitutingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.resolver == nil || t.resolver.vault == nil {
		return t.next.RoundTrip(req)
	}
	ctx := req.Context()

	// URL substitution is intentionally NOT done at the RoundTripper
	// layer: by the time we see req.URL the string has already been
	// percent-encoded by net/url (`${...}` → `%7B...%7D`) so a regex
	// scan over req.URL.String() misses it. Pack handlers that want
	// to embed placeholders in URLs must call Substitute() on the
	// raw URL string BEFORE handing it to http.NewRequest. The
	// http.fetch pack does exactly that.
	//
	// This RoundTripper handles the two cases agents most commonly
	// hit: Authorization headers and JSON request bodies.

	// 1. Headers — every value of every header gets scanned. The
	// most common case is Authorization: Bearer ${vault:foo}.
	for name, values := range req.Header {
		for i, v := range values {
			rewritten, err := t.resolver.Substitute(ctx, t.actor, v)
			if err != nil {
				return nil, err
			}
			req.Header[name][i] = rewritten
		}
	}

	// 2. Body — only when the body is small enough to buffer
	// safely. Larger bodies pass through with a warning.
	if req.Body != nil && req.ContentLength >= 0 && req.ContentLength <= bodyScanLimit {
		raw, err := io.ReadAll(io.LimitReader(req.Body, bodyScanLimit+1))
		if err != nil {
			return nil, fmt.Errorf("placeholder: reading body: %w", err)
		}
		_ = req.Body.Close()
		if len(raw) > bodyScanLimit {
			t.resolver.logger.Warn("placeholder: request body exceeds scan limit, forwarding unchanged",
				"limit", bodyScanLimit, "size", len(raw))
			req.Body = io.NopCloser(bytes.NewReader(raw))
		} else {
			rewritten, err := t.resolver.Substitute(ctx, t.actor, string(raw))
			if err != nil {
				return nil, err
			}
			req.Body = io.NopCloser(strings.NewReader(rewritten))
			req.ContentLength = int64(len(rewritten))
		}
	}

	return t.next.RoundTrip(req)
}
