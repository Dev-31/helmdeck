-- T607: provider_calls captures every /v1/chat/completions dispatch
-- the gateway routes to a real provider. Populated by
-- internal/gateway.SQLiteRecorder, queried by GET /api/v1/providers/stats
-- to compute per-(provider, model) success rates and latency rollups
-- for the AI Providers panel's "Model Success Rates" tab.
--
-- The table is intentionally separate from audit_log: audit_log
-- is the operator-facing forensic record (every API call, every
-- vault read, redacted payloads) and lives behind the Audit Logs
-- panel. provider_calls is a *metrics* table — narrow schema, no
-- payloads, no actor identity, just enough columns to compute
-- aggregates over a time window.
--
-- status is one of:
--   'success'  — provider returned a 2xx + parseable body
--   'error'    — provider returned 4xx/5xx OR network/decode failure
-- error_code is non-empty only when status='error'; the value comes
-- from gateway.classifyError() and is one of a small closed set
-- (network, timeout, http_4xx, http_5xx, decode, unknown_provider).
--
-- fallback_used is true when this record is the *fallback* leg of a
-- chain that triggered failover — operators query this to see how
-- often a primary provider is failing badly enough to trip the chain.
-- A primary call that succeeds on the first try has fallback_used=0.

CREATE TABLE IF NOT EXISTS provider_calls (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                TEXT    NOT NULL,
    provider          TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    status            TEXT    NOT NULL,
    latency_ms        INTEGER NOT NULL,
    error_code        TEXT,
    fallback_used     INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0
);

-- The success-rate aggregation query is roughly:
--   SELECT provider, model, COUNT(*), SUM(status='success'), AVG(latency_ms)
--   FROM provider_calls
--   WHERE ts >= ?
--   GROUP BY provider, model
-- so the (provider, model, ts) compound index lets the GROUP BY
-- and the WHERE both use the index. ts on its own covers the
-- "everything in window" path.
CREATE INDEX IF NOT EXISTS idx_provider_calls_ts             ON provider_calls(ts);
CREATE INDEX IF NOT EXISTS idx_provider_calls_provider_model ON provider_calls(provider, model, ts);
