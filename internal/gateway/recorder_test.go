// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package gateway

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/tosin2013/helmdeck/internal/store"
)

func TestSQLiteRecorder_RoundTrip(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r := NewSQLiteRecorder(db)
	ctx := context.Background()

	if err := r.Record(ctx, CallRecord{
		Provider:         "openai",
		Model:            "gpt-4o",
		Status:           "success",
		LatencyMS:        123,
		PromptTokens:     10,
		CompletionTokens: 20,
		TotalTokens:      30,
	}); err != nil {
		t.Fatalf("record success: %v", err)
	}
	if err := r.Record(ctx, CallRecord{
		Provider:  "openai",
		Model:     "gpt-4o",
		Status:    "error",
		LatencyMS: 50,
		ErrorCode: "http_5xx",
	}); err != nil {
		t.Fatalf("record error: %v", err)
	}

	var (
		total   int
		success int
		latency float64
	)
	row := db.QueryRow(`
        SELECT COUNT(*),
               SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),
               AVG(latency_ms)
        FROM provider_calls`)
	if err := row.Scan(&total, &success, &latency); err != nil {
		t.Fatal(err)
	}
	if total != 2 || success != 1 {
		t.Errorf("total=%d success=%d, want 2/1", total, success)
	}
	if latency == 0 {
		t.Errorf("avg latency unexpectedly zero")
	}
}

func TestSQLiteRecorder_DefaultsTimestamp(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r := NewSQLiteRecorder(db)
	if err := r.Record(context.Background(), CallRecord{
		Provider: "anthropic", Model: "claude-opus", Status: "success", LatencyMS: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var ts string
	if err := db.QueryRow(`SELECT ts FROM provider_calls LIMIT 1`).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	got, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("ts not RFC3339Nano: %q (%v)", ts, err)
	}
	if time.Since(got) > 5*time.Second {
		t.Errorf("auto-stamped ts is %v old, want recent", time.Since(got))
	}
}

func TestClassifyRecordError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"5xx", &providerError{Provider: "openai", StatusCode: 503}, "http_5xx"},
		{"4xx", &providerError{Provider: "openai", StatusCode: 429}, "http_4xx"},
		{"unknown_provider", ErrUnknownProvider, "unknown_provider"},
		{"net error timeout", &fakeNetErr{timeout: true}, "timeout"},
		{"net error generic", &fakeNetErr{}, "network"},
		{"decode", errors.New("could not decode response"), "decode"},
		{"unknown", errors.New("something else"), "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyRecordError(c.err)
			if got != c.want {
				t.Errorf("classifyRecordError(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// fakeNetErr satisfies net.Error so we can drive the timeout vs
// non-timeout branches of classifyRecordError without hitting an
// actual network.
type fakeNetErr struct {
	timeout bool
}

func (e *fakeNetErr) Error() string   { return "fake net error" }
func (e *fakeNetErr) Timeout() bool   { return e.timeout }
func (e *fakeNetErr) Temporary() bool { return false }

// Compile-time assertion that fakeNetErr satisfies net.Error.
var _ net.Error = (*fakeNetErr)(nil)
