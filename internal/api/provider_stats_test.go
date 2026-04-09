// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tosin2013/helmdeck/internal/store"
)

func TestProviderStats_AggregatesAndSuccessRate(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Insert: openai/gpt-4o has 3 success + 1 error (75%);
	// anthropic/claude-opus has 2 success (100%).
	now := time.Now().UTC()
	rows := []struct {
		provider, model, status, errCode string
		latency                          int64
	}{
		{"openai", "gpt-4o", "success", "", 120},
		{"openai", "gpt-4o", "success", "", 200},
		{"openai", "gpt-4o", "success", "", 160},
		{"openai", "gpt-4o", "error", "http_5xx", 50},
		{"anthropic", "claude-opus", "success", "", 800},
		{"anthropic", "claude-opus", "success", "", 900},
	}
	for _, r := range rows {
		var errCode any
		if r.errCode != "" {
			errCode = r.errCode
		}
		_, err := db.Exec(`
            INSERT INTO provider_calls
                (ts, provider, model, status, latency_ms, error_code, fallback_used,
                 prompt_tokens, completion_tokens, total_tokens)
            VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, 0)`,
			now.Format(time.RFC3339Nano), r.provider, r.model, r.status, r.latency, errCode)
		if err != nil {
			t.Fatal(err)
		}
	}

	h := NewRouter(Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
		DB:      db,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/providers/stats?window=1h", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var got providerStatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 2 {
		t.Fatalf("count=%d want 2 (one row per provider/model bucket)", got.Count)
	}
	// Rows are ordered by total DESC.
	if got.Rows[0].Provider != "openai" || got.Rows[0].Model != "gpt-4o" {
		t.Errorf("first row=%+v want openai/gpt-4o", got.Rows[0])
	}
	if got.Rows[0].Total != 4 || got.Rows[0].Success != 3 || got.Rows[0].Errors != 1 {
		t.Errorf("openai counts=%+v", got.Rows[0])
	}
	if got.Rows[0].SuccessRate != 0.75 {
		t.Errorf("openai success_rate=%v want 0.75", got.Rows[0].SuccessRate)
	}
	if got.Rows[1].Provider != "anthropic" || got.Rows[1].SuccessRate != 1.0 {
		t.Errorf("anthropic row=%+v", got.Rows[1])
	}
}

func TestProviderStats_NoDB_EmptyRows(t *testing.T) {
	h := NewRouter(Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
		// no DB — endpoint should return empty rows, not 500
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/providers/stats", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got providerStatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 0 || len(got.Rows) != 0 {
		t.Errorf("expected empty rows on no-DB path, got %+v", got)
	}
}

func TestProviderStats_BadWindowRejected(t *testing.T) {
	h := NewRouter(Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/providers/stats?window=banana", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rr.Code)
	}
}
