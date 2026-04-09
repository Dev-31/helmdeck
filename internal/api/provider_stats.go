// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// providerStatsRow is one row in the success-rate aggregation
// returned by GET /api/v1/providers/stats. One row per
// (provider, model) bucket within the requested time window.
type providerStatsRow struct {
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Total        int64   `json:"total"`
	Success      int64   `json:"success"`
	Errors       int64   `json:"errors"`
	SuccessRate  float64 `json:"success_rate"` // 0.0–1.0
	AvgLatencyMS int64   `json:"avg_latency_ms"`
	LastSeen     string  `json:"last_seen"` // RFC3339Nano UTC
}

type providerStatsResponse struct {
	Window string             `json:"window"`
	From   string             `json:"from"`
	Rows   []providerStatsRow `json:"rows"`
	Count  int                `json:"count"`
}

// registerProviderStatsRoutes mounts GET /api/v1/providers/stats
// (T607). Aggregates the provider_calls table by (provider, model)
// over a configurable time window. Used by the AI Providers panel's
// "Model Success Rates" tab.
//
// Query params:
//
//	?window=24h   Go duration format (default 24h, max 720h = 30d)
//
// The endpoint requires deps.DB. When DB is nil it returns an empty
// rows array — the panel renders a "no data yet" empty state, which
// is the right behavior on a fresh install where no /v1/chat/completions
// requests have been made.
func registerProviderStatsRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/v1/providers/stats", func(w http.ResponseWriter, r *http.Request) {
		windowStr := r.URL.Query().Get("window")
		if windowStr == "" {
			windowStr = "24h"
		}
		window, err := time.ParseDuration(windowStr)
		if err != nil || window <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_input",
				"window must be a positive Go duration (e.g. 24h, 1h, 7d as 168h)")
			return
		}
		// Hard cap at 30 days. Without it an operator running
		// `?window=10000h` could pin the SQLite query for seconds
		// against a long-lived control plane.
		if window > 30*24*time.Hour {
			window = 30 * 24 * time.Hour
		}

		from := time.Now().UTC().Add(-window)
		resp := providerStatsResponse{
			Window: window.String(),
			From:   from.Format(time.RFC3339Nano),
			Rows:   []providerStatsRow{},
		}

		if deps.DB == nil {
			resp.Count = 0
			writeJSON(w, http.StatusOK, resp)
			return
		}

		rows, err := queryProviderStats(r.Context(), deps.DB, from)
		if err != nil {
			deps.Logger.Error("provider stats query failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"provider stats query failed")
			return
		}
		resp.Rows = rows
		resp.Count = len(rows)
		writeJSON(w, http.StatusOK, resp)
	})
}

// queryProviderStats runs the aggregation against the SQLite
// provider_calls table. Lives in this file (not internal/store)
// because the result type is API-shaped — moving it into store
// would mean a circular import or a duplicated row struct.
func queryProviderStats(ctx context.Context, db *sql.DB, from time.Time) ([]providerStatsRow, error) {
	const q = `
        SELECT
            provider,
            model,
            COUNT(*)                                                  AS total,
            SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END)       AS success,
            SUM(CASE WHEN status = 'error'   THEN 1 ELSE 0 END)       AS errors,
            CAST(AVG(latency_ms) AS INTEGER)                          AS avg_latency_ms,
            MAX(ts)                                                   AS last_seen
        FROM provider_calls
        WHERE ts >= ?
        GROUP BY provider, model
        ORDER BY total DESC`
	rs, err := db.QueryContext(ctx, q, from.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	var out []providerStatsRow
	for rs.Next() {
		var (
			row      providerStatsRow
			lastSeen string
		)
		if err := rs.Scan(
			&row.Provider, &row.Model,
			&row.Total, &row.Success, &row.Errors,
			&row.AvgLatencyMS, &lastSeen,
		); err != nil {
			return nil, err
		}
		if row.Total > 0 {
			row.SuccessRate = float64(row.Success) / float64(row.Total)
		}
		row.LastSeen = lastSeen
		out = append(out, row)
	}
	return out, rs.Err()
}
