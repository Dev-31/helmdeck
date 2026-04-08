package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/placeholder"
	"github.com/tosin2013/helmdeck/internal/security"
	"github.com/tosin2013/helmdeck/internal/vault"
)

// HTTPFetch (T504, ADR 007) is the canonical placeholder-token
// demo: agents make HTTP calls with vault references in their
// Authorization headers and request bodies, and helmdeck
// substitutes the real credentials before forwarding. The agent
// receives the response — but never the credential.
//
// Input shape:
//
//	{
//	  "url":     "https://api.github.com/repos/foo/bar",
//	  "method":  "GET",                              // optional, default GET
//	  "headers": {                                    // optional
//	    "Authorization": "Bearer ${vault:github-token}",
//	    "Accept":        "application/vnd.github+json"
//	  },
//	  "body":    "{\"q\":\"...\"}"                    // optional, string
//	}
//
// Output shape:
//
//	{
//	  "status":  200,
//	  "headers": {"content-type": "application/json"},
//	  "body":    "..."
//	}
//
// Security:
//
//   - The egress guard (T508) blocks any URL whose host resolves
//     to cloud metadata, RFC 1918, loopback, or other private
//     ranges. Operators with internal hosts allowlist them via
//     HELMDECK_EGRESS_ALLOWLIST.
//   - Placeholder tokens (${vault:NAME}) in the URL, headers, and
//     body are substituted by the placeholder.Resolver before the
//     request leaves helmdeck. Unknown names fail the request with
//     CodeInvalidInput; ACL-denied names fail with the same code.
//   - Response body is capped at maxResponseBytes to prevent OOM
//     when an agent points the pack at a multi-gigabyte download.
const maxResponseBytes = 16 << 20 // 16 MiB

func HTTPFetch(v *vault.Store, eg *security.EgressGuard) *packs.Pack {
	return &packs.Pack{
		Name:        "http.fetch",
		Version:     "v1",
		Description: "Make an HTTP request with vault placeholder substitution and egress guard.",
		InputSchema: packs.BasicSchema{
			Required: []string{"url"},
			Properties: map[string]string{
				"url":     "string",
				"method":  "string",
				"headers": "object",
				"body":    "string",
			},
		},
		OutputSchema: packs.BasicSchema{
			Required: []string{"status", "body"},
			Properties: map[string]string{
				"status":  "number",
				"headers": "object",
				"body":    "string",
			},
		},
		Handler: httpFetchHandler(v, eg),
	}
}

type httpFetchInput struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func httpFetchHandler(v *vault.Store, eg *security.EgressGuard) packs.HandlerFunc {
	return func(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
		var in httpFetchInput
		if err := json.Unmarshal(ec.Input, &in); err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
		}
		if strings.TrimSpace(in.URL) == "" {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "url is required"}
		}
		method := strings.ToUpper(strings.TrimSpace(in.Method))
		if method == "" {
			method = "GET"
		}
		// Closed-set methods — if an agent wants something exotic
		// (CONNECT, PATCH+body), they can request a follow-up.
		switch method {
		case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD":
		default:
			return nil, &packs.PackError{Code: packs.CodeInvalidInput,
				Message: fmt.Sprintf("unsupported method %q", method)}
		}

		// Build the placeholder resolver. Wildcard actor for now —
		// engine actor threading is the same follow-up that
		// repo.fetch and repo.push are waiting on.
		actor := vault.Actor{Subject: "*"}
		resolver := placeholder.New(v, ec.Logger)

		// 1. Substitute the URL string BEFORE parsing — the resolver
		// can't see ${...} after net/url percent-encodes it.
		url, err := resolver.Substitute(ctx, actor, in.URL)
		if err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput,
				Message: fmt.Sprintf("url substitution: %v", err), Cause: err}
		}

		// 2. Egress guard. Refuse hosts that resolve to blocked ranges.
		if eg != nil {
			if err := eg.CheckURL(ctx, url); err != nil {
				return nil, &packs.PackError{Code: packs.CodeInvalidInput,
					Message: fmt.Sprintf("egress denied: %v", err), Cause: err}
			}
		}

		// 3. Build the request. Headers and body are substituted
		// by the placeholder-aware http.Client below — we copy
		// them onto the request verbatim with the placeholder
		// patterns intact.
		var body io.Reader
		if in.Body != "" {
			body = bytes.NewReader([]byte(in.Body))
		}
		req, err := http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput,
				Message: fmt.Sprintf("build request: %v", err), Cause: err}
		}
		for k, val := range in.Headers {
			req.Header.Set(k, val)
		}

		// 4. Dispatch via the placeholder-aware client.
		client := resolver.HTTPClient(actor)
		resp, err := client.Do(req)
		if err != nil {
			// Distinguish between "credential resolution failed"
			// (returned from the RoundTripper) and "real network
			// failure". The first is invalid_input; the second is
			// handler_failed.
			if errors.Is(err, placeholder.ErrUnknownPlaceholder) || errors.Is(err, placeholder.ErrDenied) {
				return nil, &packs.PackError{Code: packs.CodeInvalidInput,
					Message: err.Error(), Cause: err}
			}
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
				Message: fmt.Sprintf("http request: %v", err), Cause: err}
		}
		defer resp.Body.Close()

		// 5. Buffer the response body up to the cap. A larger
		// response is truncated and a flag set in the output so
		// the agent can decide whether to retry with a narrower
		// query.
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if err != nil {
			return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
				Message: fmt.Sprintf("read response: %v", err), Cause: err}
		}
		truncated := false
		if len(respBody) > maxResponseBytes {
			respBody = respBody[:maxResponseBytes]
			truncated = true
		}

		// 6. Flatten headers to a string→string map (taking the
		// first value of each multi-valued header) so the JSON
		// shape stays simple.
		hdrs := make(map[string]string, len(resp.Header))
		for k, values := range resp.Header {
			if len(values) > 0 {
				hdrs[k] = values[0]
			}
		}
		return json.Marshal(map[string]any{
			"status":    resp.StatusCode,
			"headers":   hdrs,
			"body":      string(respBody),
			"truncated": truncated,
		})
	}
}
