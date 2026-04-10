// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package api

// webhook_github.go (ADR 033) — GitHub webhook receiver.
//
// POST /api/v1/webhooks/github receives push/PR events from GitHub,
// validates the HMAC-SHA256 signature against HELMDECK_GITHUB_WEBHOOK_SECRET,
// and dispatches the configured pack. The endpoint returns 200
// immediately (async dispatch) so GitHub doesn't time out.
//
// This is Phase 1: push + pull_request events only, single-pack
// dispatch per rule, rules from env var. Future phases add rule CRUD,
// pack chaining, and commit-status posting.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// WebhookRule maps a GitHub event to a pack dispatch.
type WebhookRule struct {
	Event  string          `json:"event"`            // "push", "pull_request"
	Ref    string          `json:"ref,omitempty"`     // optional ref filter, e.g. "refs/heads/main"
	Action string          `json:"action,omitempty"`  // optional action filter, e.g. "opened"
	Pack   string          `json:"pack"`              // pack to dispatch, e.g. "cmd.run"
	Args   json.RawMessage `json:"args,omitempty"`    // pack input merged with event metadata
}

const maxWebhookPayload = 5 << 20 // 5 MiB

func registerGitHubWebhookRoute(mux *http.ServeMux, deps Deps) {
	secret := os.Getenv("HELMDECK_GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		// Read from _FILE variant
		if path := os.Getenv("HELMDECK_GITHUB_WEBHOOK_SECRET_FILE"); path != "" {
			b, err := os.ReadFile(path)
			if err == nil {
				secret = strings.TrimSpace(string(b))
			}
		}
	}

	mux.HandleFunc("POST /api/v1/webhooks/github", func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			writeError(w, http.StatusServiceUnavailable, "webhook_not_configured",
				"HELMDECK_GITHUB_WEBHOOK_SECRET not set")
			return
		}

		// Read + validate payload
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookPayload+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read_body", err.Error())
			return
		}
		if len(body) > maxWebhookPayload {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("payload exceeds %d bytes", maxWebhookPayload))
			return
		}

		// HMAC-SHA256 signature validation
		sig := r.Header.Get("X-Hub-Signature-256")
		if sig == "" {
			writeError(w, http.StatusUnauthorized, "missing_signature",
				"X-Hub-Signature-256 header required")
			return
		}
		if !verifyGitHubSignature(secret, sig, body) {
			writeError(w, http.StatusUnauthorized, "invalid_signature",
				"HMAC signature verification failed")
			return
		}

		eventType := r.Header.Get("X-GitHub-Event")
		deliveryID := r.Header.Get("X-GitHub-Delivery")

		// Parse rules from env
		rules := parseWebhookRules()
		if len(rules) == 0 {
			deps.Logger.Info("github webhook received but no rules configured",
				"event", eventType, "delivery", deliveryID)
			writeJSON(w, http.StatusOK, map[string]string{
				"status":   "accepted",
				"message":  "no rules configured — event logged but not dispatched",
				"delivery": deliveryID,
			})
			return
		}

		// Parse event payload for ref/action filtering
		var payload struct {
			Ref    string `json:"ref"`
			Action string `json:"action"`
			Repo   struct {
				FullName string `json:"full_name"`
				CloneURL string `json:"clone_url"`
			} `json:"repository"`
		}
		_ = json.Unmarshal(body, &payload)

		// Match rules and dispatch
		matched := 0
		for _, rule := range rules {
			if rule.Event != eventType {
				continue
			}
			if rule.Ref != "" && rule.Ref != payload.Ref {
				continue
			}
			if rule.Action != "" && rule.Action != payload.Action {
				continue
			}

			matched++
			deps.Logger.Info("github webhook dispatching pack",
				"event", eventType,
				"ref", payload.Ref,
				"repo", payload.Repo.FullName,
				"pack", rule.Pack,
				"delivery", deliveryID,
			)

			// Async dispatch — don't block the webhook response
			go func(rule WebhookRule) {
				if deps.PackRegistry == nil || deps.PackEngine == nil {
					deps.Logger.Warn("webhook dispatch skipped: pack registry not configured")
					return
				}
				pack, err := deps.PackRegistry.Get(rule.Pack, "")
				if err != nil {
					deps.Logger.Warn("webhook dispatch: pack not found",
						"pack", rule.Pack, "err", err)
					return
				}
				// Build input: merge rule args with event metadata
				input := rule.Args
				if len(input) == 0 {
					input = json.RawMessage(fmt.Sprintf(
						`{"url":"%s","ref":"%s","_webhook_event":"%s","_webhook_delivery":"%s"}`,
						payload.Repo.CloneURL, payload.Ref, eventType, deliveryID,
					))
				}
				_, err = deps.PackEngine.Execute(r.Context(), pack, input)
				if err != nil {
					deps.Logger.Warn("webhook dispatch: pack failed",
						"pack", rule.Pack, "err", err)
				} else {
					deps.Logger.Info("webhook dispatch: pack succeeded",
						"pack", rule.Pack, "delivery", deliveryID)
				}
			}(rule)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":       "accepted",
			"event":        eventType,
			"delivery":     deliveryID,
			"rules_matched": matched,
		})
	})
}

func verifyGitHubSignature(secret, signature string, payload []byte) bool {
	// GitHub sends "sha256=<hex>"
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	expected := strings.TrimPrefix(signature, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	actual := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(actual))
}

func parseWebhookRules() []WebhookRule {
	raw := os.Getenv("HELMDECK_GITHUB_WEBHOOK_RULES")
	if raw == "" {
		return nil
	}
	var rules []WebhookRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil
	}
	return rules
}
