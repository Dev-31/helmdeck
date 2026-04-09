// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tosin2013/helmdeck/internal/audit"
)

// auditEntryResponse is the JSON shape returned by GET /api/v1/audit.
// We re-shape audit.Entry so the wire format is stable even if the
// internal struct grows new fields. Timestamps are RFC3339Nano UTC
// strings to match the rest of the API surface.
type auditEntryResponse struct {
	ID           int64          `json:"id"`
	Timestamp    string         `json:"timestamp"`
	Severity     string         `json:"severity"`
	EventType    string         `json:"event_type"`
	ActorSubject string         `json:"actor_subject,omitempty"`
	ActorClient  string         `json:"actor_client,omitempty"`
	SessionID    string         `json:"session_id,omitempty"`
	Method       string         `json:"method,omitempty"`
	Path         string         `json:"path,omitempty"`
	StatusCode   int            `json:"status_code"`
	Payload      map[string]any `json:"payload,omitempty"`
}

type auditListResponse struct {
	Entries []auditEntryResponse `json:"entries"`
	Count   int                  `json:"count"`
}

// registerAuditRoutes mounts GET /api/v1/audit (T611). Operators
// query the audit log via the Management UI's Audit Logs panel; the
// same endpoint backs CLI / scripted forensic queries. The audit
// writer is plumbed via deps.Audit and is always non-nil after
// router.go's Discard fallback, so this handler safely calls Query.
//
// Filters are read from query params and map 1:1 onto audit.Filter:
//
//	?from=2026-04-01T00:00:00Z   inclusive lower bound (RFC3339)
//	?to=2026-04-09T23:59:59Z     inclusive upper bound (RFC3339)
//	?event_type=login            exact match on event_type
//	?session_id=abc123           exact match on session_id
//	?actor_subject=admin         exact match on actor_subject (JWT sub)
//	?severity=warn               exact match on severity
//	?limit=100                   max rows (default 100, max 1000)
//
// The handler clamps limit to [1, 1000] so a runaway operator query
// can't OOM the control plane on a long-lived audit table.
func registerAuditRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		f := audit.Filter{}
		q := r.URL.Query()

		if v := q.Get("from"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_input",
					"from must be RFC3339 (e.g. 2026-04-09T00:00:00Z)")
				return
			}
			f.From = t
		}
		if v := q.Get("to"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_input",
					"to must be RFC3339 (e.g. 2026-04-09T23:59:59Z)")
				return
			}
			f.To = t
		}
		if v := q.Get("event_type"); v != "" {
			f.EventType = audit.EventType(v)
		}
		if v := q.Get("session_id"); v != "" {
			f.SessionID = v
		}
		if v := q.Get("actor_subject"); v != "" {
			f.ActorSubject = v
		}
		if v := q.Get("severity"); v != "" {
			f.Severity = audit.Severity(v)
		}

		// Default limit 100 — enough to fill the panel's first page
		// without dragging the whole table on every poll. Hard cap
		// 1000 so a hand-crafted ?limit=1000000 can't run away.
		limit := 100
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				writeError(w, http.StatusBadRequest, "invalid_input",
					"limit must be a positive integer")
				return
			}
			if n > 1000 {
				n = 1000
			}
			limit = n
		}
		f.Limit = limit

		entries, err := deps.Audit.Query(r.Context(), f)
		if err != nil {
			deps.Logger.Error("audit query failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"audit query failed")
			return
		}

		resp := auditListResponse{
			Entries: make([]auditEntryResponse, 0, len(entries)),
			Count:   len(entries),
		}
		for _, e := range entries {
			resp.Entries = append(resp.Entries, auditEntryResponse{
				ID:           e.ID,
				Timestamp:    e.Timestamp.UTC().Format(time.RFC3339Nano),
				Severity:     string(e.Severity),
				EventType:    string(e.EventType),
				ActorSubject: e.ActorSubject,
				ActorClient:  e.ActorClient,
				SessionID:    e.SessionID,
				Method:       e.Method,
				Path:         e.Path,
				StatusCode:   e.StatusCode,
				Payload:      e.Payload,
			})
		}
		writeJSON(w, http.StatusOK, resp)
	})
}
