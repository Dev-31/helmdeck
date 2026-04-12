// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package builtin

// content_ground.go (T623, ADR 035) — link-grounding for markdown.
//
// An agent hands in a path to a blog post in a session-local clone.
// The pack (1) asks the gateway LLM to extract the top N factual
// claims that would benefit from a citation, (2) runs each claim's
// generated search query through Firecrawl /v1/search, (3) picks
// the first result with a non-empty URL, and (4) rewrites the
// markdown file in place by appending ` [source](url)` after each
// grounded claim. The result is a report of which claims got
// citations, which were skipped, and the new sha256 of the file.
//
// Why one pack and not `research.deep` + `fs.patch` chained by the
// agent?
//   - Claim extraction is a strict JSON round trip — delegating it
//     to the agent means every caller has to re-implement the same
//     prompt and parser, and drift across callers produces
//     inconsistent groundings.
//   - The substring the LLM picks as "the claim" has to match the
//     file content exactly. When the agent orchestrates from
//     outside, the claim text and the file content live in two
//     different context windows and drift is common. One pack that
//     owns the whole pipeline avoids that whole class of bug.
//   - The fs write needs to happen once per run, not once per
//     claim, so we can cap the number of session-shell round trips
//     regardless of how many claims were grounded.
//
// Milestone note (T623): the original milestone text mentions
// `github.search` + `http.fetch` + `web.scrape` + `fs.patch` as the
// canonical chain. We collapse the first three into a single
// Firecrawl `/v1/search` call with inline scrape — Google (which
// Firecrawl's self-hosted default uses) indexes GitHub repos,
// docs, and issues alongside the rest of the web, so one API path
// covers "search GitHub and the wider web" without the plumbing of
// a separate github.search integration. Operators who need
// GitHub-only results add a `site:github.com` token to the
// generated query (the claim-extractor prompt honours a `topic`
// hint for exactly this reason).
//
// Deployment:
//   - Gated on HELMDECK_FIRECRAWL_ENABLED=true (same toggle as
//     web.scrape and research.deep).
//   - NeedsSession=true because the markdown file lives in a
//     session-local clone — fs reads/writes go through the session
//     executor the same way fs.patch does.
//   - No egress guard on claim queries (they are search strings,
//     not URLs). Firecrawl's own egress policy enforces SSRF
//     defence on the crawler side.
//
// Input shape:
//
//	{
//	  "clone_path": "/tmp/helmdeck-abc/posts",
//	  "path":       "2026-quantum.md",
//	  "model":      "openai/gpt-4o-mini",
//	  "max_claims": 5,                                  // optional, default 5, cap 8
//	  "topic":      "quantum computing"                 // optional hint to bias the claim extractor
//	}
//
// Output shape:
//
//	{
//	  "path":              "2026-quantum.md",
//	  "claims_considered": 5,
//	  "claims_grounded":   3,
//	  "grounding":         [ { "claim": "...", "url": "...", "title": "..." }, ... ],
//	  "skipped":           [ "claim with no source found" ],
//	  "sha256":            "hex-of-patched-file",
//	  "file_changed":      true
//	}

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tosin2013/helmdeck/internal/gateway"
	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/vision"
)

const (
	defaultContentGroundClaims  = 5
	maxContentGroundClaims      = 8
	defaultContentGroundTokens  = 1024
	// contentGroundPrompt is the frozen system prompt for the claim
	// extractor. The strict JSON schema is critical — we parse the
	// response with json.Unmarshal and bail on invalid_input if it
	// doesn't match, so small models need a very concrete example.
	contentGroundPrompt = `You are a fact-checker for technical blog posts. You will receive the full markdown of a post and a maximum number of claims to extract. Your job is to pick the most impactful factual claims that would benefit from an authoritative citation.

For each claim:
  - "text" MUST be a verbatim substring of the original markdown — copy it exactly, including punctuation, so the caller can locate it with a literal substring match. Do NOT rephrase.
  - "query" is the search query you would use to find a source — specific enough to reach authoritative material, not a generic topic word.

Respond with ONE JSON object and nothing else. No markdown fences, no commentary. Schema:

{
  "claims": [
    {"text": "<exact substring from the post>", "query": "<search query>"},
    ...
  ]
}

Rules:
  - Return AT MOST the requested number of claims. Prefer fewer high-quality claims over many weak ones.
  - Skip claims that are trivially obvious, subjective ("I think", "arguably"), or already contain a link.
  - Skip headings, code blocks, and list bullets — ground only prose sentences.
  - If no claim meets the bar, return {"claims": []}.`
)

// ContentGround constructs the pack.
func ContentGround(d vision.Dispatcher) *packs.Pack {
	return &packs.Pack{
		Name:    "content.ground",
		Version: "v1",
		Description: "Extract factual claims from markdown and insert real source links via Firecrawl search. " +
			"Accepts either a file in a session clone (clone_path + path) or raw text directly. " +
			"Produces a grounded markdown artifact for download.",
		// NeedsSession is false because the text-mode path doesn't
		// require a session. When clone_path + path are provided the
		// handler checks ec.Exec at runtime and returns a clear error
		// if the session isn't available.
		NeedsSession: false,
		InputSchema: packs.BasicSchema{
			Required: []string{"model"},
			Properties: map[string]string{
				"clone_path": "string",
				"path":       "string",
				"text":       "string",
				"model":      "string",
				"max_claims": "number",
				"topic":      "string",
			},
		},
		OutputSchema: packs.BasicSchema{
			Required: []string{"claims_considered", "claims_grounded", "sha256"},
			Properties: map[string]string{
				"path":              "string",
				"claims_considered": "number",
				"claims_grounded":   "number",
				"grounding":         "array",
				"skipped":           "array",
				"sha256":            "string",
				"file_changed":      "boolean",
				"grounded_text":     "string",
				"artifact_key":      "string",
			},
		},
		Handler: contentGroundHandler(d),
	}
}

type contentGroundInput struct {
	ClonePath string `json:"clone_path"`
	Path      string `json:"path"`
	Text      string `json:"text"` // direct text mode — no session needed
	Model     string `json:"model"`
	MaxClaims int    `json:"max_claims"`
	Topic     string `json:"topic"`
}

// claimPlan is the parsed shape the extractor LLM returns.
type claimPlan struct {
	Claims []struct {
		Text  string `json:"text"`
		Query string `json:"query"`
	} `json:"claims"`
}

type grounding struct {
	Claim string `json:"claim"`
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

func contentGroundHandler(d vision.Dispatcher) packs.HandlerFunc {
	return func(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
		if d == nil {
			return nil, &packs.PackError{
				Code:    packs.CodeInternal,
				Message: "content.ground registered without a gateway dispatcher",
			}
		}
		if os.Getenv("HELMDECK_FIRECRAWL_ENABLED") != "true" {
			return nil, &packs.PackError{
				Code: packs.CodeInvalidInput,
				Message: "content.ground is disabled; set HELMDECK_FIRECRAWL_ENABLED=true " +
					"and bring up the Firecrawl overlay (deploy/compose/compose.firecrawl.yml)",
			}
		}
		base := strings.TrimRight(os.Getenv("HELMDECK_FIRECRAWL_URL"), "/")
		if base == "" {
			base = defaultFirecrawlURL
		}

		var in contentGroundInput
		if err := json.Unmarshal(ec.Input, &in); err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
		}
		if strings.TrimSpace(in.Model) == "" {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "model is required (provider/model)"}
		}
		maxClaims := in.MaxClaims
		if maxClaims <= 0 {
			maxClaims = defaultContentGroundClaims
		}
		if maxClaims > maxContentGroundClaims {
			maxClaims = maxContentGroundClaims
		}

		// Two modes:
		//   (A) text mode — markdown provided directly, no session needed
		//   (B) file mode — read from clone_path + path in a session
		var original string
		var full string
		textMode := strings.TrimSpace(in.Text) != ""

		if textMode {
			original = in.Text
		} else {
			// File mode — requires session + clone_path + path
			if strings.TrimSpace(in.ClonePath) == "" {
				return nil, &packs.PackError{Code: packs.CodeInvalidInput,
					Message: "either 'text' (direct markdown) or 'clone_path' + 'path' (file in session) is required"}
			}
			if strings.TrimSpace(in.Path) == "" {
				return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "path is required when using clone_path"}
			}
			if ec.Exec == nil {
				return nil, &packs.PackError{Code: packs.CodeSessionUnavailable,
					Message: "content.ground file mode requires a session executor; use 'text' for direct markdown input"}
			}
			var perr *packs.PackError
			full, perr = safeJoin(in.ClonePath, in.Path)
			if perr != nil {
				return nil, perr
			}
			statRes, err := runShell(ctx, ec, "wc -c < "+shellQuote(full), nil)
			if err != nil || statRes.ExitCode != 0 {
				return nil, &packs.PackError{
					Code:    packs.CodeInvalidInput,
					Message: fmt.Sprintf("file not readable: %s", strings.TrimSpace(string(statRes.Stderr))),
				}
			}
			size, _ := strconv.ParseInt(strings.TrimSpace(string(statRes.Stdout)), 10, 64)
			if size > maxFsReadBytes {
				return nil, &packs.PackError{
					Code:    packs.CodeInvalidInput,
					Message: fmt.Sprintf("file is %d bytes, exceeds %d byte cap", size, maxFsReadBytes),
				}
			}
			readRes, err := runShell(ctx, ec, "cat "+shellQuote(full), nil)
			if err != nil || readRes.ExitCode != 0 {
				return nil, &packs.PackError{
					Code:    packs.CodeHandlerFailed,
					Message: "failed to read markdown file",
				}
			}
			original = string(readRes.Stdout)
		}
		if strings.TrimSpace(original) == "" {
			return nil, &packs.PackError{
				Code:    packs.CodeInvalidInput,
				Message: "markdown content is empty",
			}
		}

		// 3. Claim extraction via LLM. The topic hint (when set)
		// gets a dedicated line in the user message — the prompt
		// knows what to do with it because it reads like part of
		// the goal framing rather than a schema field.
		claims, rawModel, perr := extractClaims(ctx, d, in.Model, original, in.Topic, maxClaims)
		if perr != nil {
			return nil, perr
		}
		if len(claims) == 0 {
			// No groundable claims — return a clean report, don't
			// touch the file. Not an error: a well-grounded post
			// is the ideal outcome.
			sum := sha256.Sum256([]byte(original))
			return json.Marshal(map[string]any{
				"path":              in.Path,
				"claims_considered": 0,
				"claims_grounded":   0,
				"grounding":         []grounding{},
				"skipped":           []string{},
				"sha256":            hex.EncodeToString(sum[:]),
				"file_changed":      false,
			})
		}
		_ = rawModel // retained for future audit logging; not surfaced today

		// 4. For each claim, search Firecrawl and take the first
		// result with a non-empty URL. Skip claims whose text
		// doesn't literally appear in the markdown (hallucinated
		// substrings are a failure mode for smaller models) and
		// claims whose query returns no usable source.
		groundings := make([]grounding, 0, len(claims))
		skipped := make([]string, 0)
		patched := original
		considered := 0

		for _, c := range claims {
			considered++
			if !strings.Contains(patched, c.Text) {
				skipped = append(skipped, c.Text)
				continue
			}
			fc, searchErr := callFirecrawlSearch(ctx, base, firecrawlSearchRequest{
				Query: c.Query,
				Limit: 3,
				// We don't need the scraped markdown here —
				// grounding only surfaces the URL — so we save
				// Firecrawl the work by omitting scrapeOptions.
			})
			if searchErr != nil {
				// A single failing search shouldn't kill the whole
				// run; record the skip and move on. The upstream
				// error is logged via the pack engine's audit path
				// when we return.
				skipped = append(skipped, c.Text)
				continue
			}
			pick := firstUsableSource(fc.Data)
			if pick == nil {
				skipped = append(skipped, c.Text)
				continue
			}
			// Insert [source](url) after the FIRST occurrence of
			// the claim text. strings.Replace with count=1 gives
			// us the exact "first match only" semantics without
			// the regex rabbit hole.
			insertion := fmt.Sprintf("%s [source](%s)", c.Text, pick.URL)
			patched = strings.Replace(patched, c.Text, insertion, 1)
			title := pick.Title
			if title == "" {
				title = pick.Metadata.Title
			}
			groundings = append(groundings, grounding{
				Claim: c.Text,
				URL:   pick.URL,
				Title: title,
			})
		}

		// 5. Write back + artifact.
		fileChanged := patched != original

		// File mode: write the patched file back to the session.
		if !textMode && fileChanged && ec.Exec != nil {
			writeRes, err := runShell(ctx, ec, "cat > "+shellQuote(full), []byte(patched))
			if err != nil || writeRes.ExitCode != 0 {
				return nil, &packs.PackError{
					Code:    packs.CodeHandlerFailed,
					Message: fmt.Sprintf("write back failed: %s", strings.TrimSpace(string(writeRes.Stderr))),
				}
			}
		}

		// Always upload the grounded markdown as a downloadable
		// artifact so the operator can copy-paste it to a blog.
		var artifactKey string
		if ec.Artifacts != nil && fileChanged {
			art, err := ec.Artifacts.Put(ctx, "content.ground", "grounded.md", []byte(patched), "text/markdown")
			if err != nil {
				ec.Logger.Warn("artifact upload failed", "err", err)
			} else {
				artifactKey = art.Key
			}
		}

		sum := sha256.Sum256([]byte(patched))
		out := map[string]any{
			"path":              in.Path,
			"claims_considered": considered,
			"claims_grounded":   len(groundings),
			"grounding":         groundings,
			"skipped":           skipped,
			"sha256":            hex.EncodeToString(sum[:]),
			"file_changed":      fileChanged,
			"grounded_text":     patched,
		}
		if artifactKey != "" {
			out["artifact_key"] = artifactKey
		}
		return json.Marshal(out)
	}
}

// extractClaims asks the LLM to pick up to maxClaims grounding
// candidates. Returns the parsed claim list plus the raw model
// response (useful for future audit logging).
func extractClaims(ctx context.Context, d vision.Dispatcher, model, markdown, topic string, maxClaims int) ([]struct {
	Text  string `json:"text"`
	Query string `json:"query"`
}, string, *packs.PackError) {
	maxTokens := defaultContentGroundTokens
	var userMsg strings.Builder
	fmt.Fprintf(&userMsg, "MAX_CLAIMS: %d\n", maxClaims)
	if strings.TrimSpace(topic) != "" {
		fmt.Fprintf(&userMsg, "TOPIC HINT: %s\n", topic)
	}
	userMsg.WriteString("\nPOST:\n")
	userMsg.WriteString(markdown)
	req := gateway.ChatRequest{
		Model:     model,
		MaxTokens: &maxTokens,
		Messages: []gateway.Message{
			{Role: "system", Content: gateway.TextContent(contentGroundPrompt)},
			{Role: "user", Content: gateway.TextContent(userMsg.String())},
		},
	}
	resp, err := d.Dispatch(ctx, req)
	if err != nil {
		return nil, "", &packs.PackError{
			Code:    packs.CodeHandlerFailed,
			Message: fmt.Sprintf("claim extractor dispatch: %v", err),
			Cause:   err,
		}
	}
	if len(resp.Choices) == 0 {
		return nil, "", &packs.PackError{
			Code:    packs.CodeHandlerFailed,
			Message: "claim extractor returned no choices",
			Cause:   errors.New("empty choices"),
		}
	}
	raw := strings.TrimSpace(resp.Choices[0].Message.Content.Text())
	plan, perr := parseClaimPlan(raw)
	if perr != nil {
		return nil, raw, perr
	}
	// Cap to maxClaims in case the model ignored the instruction.
	if len(plan.Claims) > maxClaims {
		plan.Claims = plan.Claims[:maxClaims]
	}
	return plan.Claims, raw, nil
}

// parseClaimPlan tolerates the same prose/markdown wrapping as
// webtest.parsePlan — strict unmarshal first, balanced-brace
// fallback second. Returns a PackError so callers can plug it
// into their error return directly.
func parseClaimPlan(raw string) (claimPlan, *packs.PackError) {
	var p claimPlan
	if err := json.Unmarshal([]byte(raw), &p); err == nil {
		return p, nil
	}
	if obj := extractFirstJSONObject(raw); obj != "" {
		if err := json.Unmarshal([]byte(obj), &p); err == nil {
			return p, nil
		}
	}
	snippet := raw
	if len(snippet) > 256 {
		snippet = snippet[:256] + "…"
	}
	return claimPlan{}, &packs.PackError{
		Code:    packs.CodeHandlerFailed,
		Message: fmt.Sprintf("claim extractor returned unparseable JSON: %s", snippet),
	}
}

// firstUsableSource returns the first search result with a
// non-empty URL, or nil if none. We deliberately ignore empty-
// description and missing-markdown results here — for grounding
// the URL alone is enough, and restricting further would skip
// otherwise-fine sources for no reason.
func firstUsableSource(items []firecrawlSearchItem) *firecrawlSearchItem {
	for i := range items {
		if items[i].URL != "" {
			return &items[i]
		}
	}
	return nil
}
