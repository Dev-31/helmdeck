// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tosin2013/helmdeck/internal/audit"
)

// stubAuditWriter is a minimal Writer that returns a canned slice
// from Query and records the Filter it was called with so the test
// can assert that query params land on the right field.
type stubAuditWriter struct {
	entries  []audit.Entry
	lastCall audit.Filter
}

func (s *stubAuditWriter) Write(context.Context, audit.Entry) error { return nil }
func (s *stubAuditWriter) Query(_ context.Context, f audit.Filter) ([]audit.Entry, error) {
	s.lastCall = f
	return s.entries, nil
}

func newAuditRouter(t *testing.T, w *stubAuditWriter) http.Handler {
	t.Helper()
	return NewRouter(Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
		Audit:   w,
		// no Issuer => /api/v1/* auth disabled (dev mode)
	})
}

func TestAuditList_DefaultLimit(t *testing.T) {
	w := &stubAuditWriter{
		entries: []audit.Entry{
			{ID: 1, Timestamp: time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC), Severity: audit.SeverityInfo, EventType: audit.EventLogin, ActorSubject: "admin", StatusCode: 200},
			{ID: 2, Timestamp: time.Date(2026, 4, 9, 12, 1, 0, 0, time.UTC), Severity: audit.SeverityWarning, EventType: audit.EventPackCall, Method: "POST", Path: "/api/v1/packs/run", StatusCode: 403},
		},
	}
	h := newAuditRouter(t, w)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if w.lastCall.Limit != 100 {
		t.Errorf("default limit=%d want 100", w.lastCall.Limit)
	}
	var got auditListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 2 || len(got.Entries) != 2 {
		t.Errorf("count=%d entries=%d, want 2/2", got.Count, len(got.Entries))
	}
	if got.Entries[0].EventType != "login" || got.Entries[0].ActorSubject != "admin" {
		t.Errorf("entry[0]=%+v", got.Entries[0])
	}
}

func TestAuditList_FiltersAndLimitClamp(t *testing.T) {
	w := &stubAuditWriter{}
	h := newAuditRouter(t, w)

	url := "/api/v1/audit?from=2026-04-01T00:00:00Z&to=2026-04-09T23:59:59Z" +
		"&event_type=pack_call&session_id=sess-abc" +
		"&actor_subject=admin&severity=warning&limit=999999"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	f := w.lastCall
	if f.From.IsZero() || f.To.IsZero() {
		t.Errorf("time bounds not parsed: from=%v to=%v", f.From, f.To)
	}
	if string(f.EventType) != "pack_call" {
		t.Errorf("event_type=%q", f.EventType)
	}
	if f.SessionID != "sess-abc" || f.ActorSubject != "admin" {
		t.Errorf("session/actor=%q/%q", f.SessionID, f.ActorSubject)
	}
	if string(f.Severity) != "warning" {
		t.Errorf("severity=%q", f.Severity)
	}
	if f.Limit != 1000 {
		t.Errorf("limit=%d, want 1000 (clamped from 999999)", f.Limit)
	}
}

func TestAuditList_BadFromRejected(t *testing.T) {
	w := &stubAuditWriter{}
	h := newAuditRouter(t, w)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/audit?from=yesterday", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
}

func TestAuditList_BadLimitRejected(t *testing.T) {
	w := &stubAuditWriter{}
	h := newAuditRouter(t, w)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=-5", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
}
