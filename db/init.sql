CREATE TABLE IF NOT EXISTS request_comparisons (
    id                   BIGSERIAL PRIMARY KEY,
    request_id           TEXT NOT NULL UNIQUE,
    -- W3C trace id, so a row can be looked up in Jaeger.
    trace_id             TEXT,
    method               TEXT NOT NULL,
    path                 TEXT NOT NULL,
    query                TEXT NOT NULL DEFAULT '',

    stable_status        INTEGER NOT NULL,
    candidate_status     INTEGER,
    stable_latency_ms    DOUBLE PRECISION NOT NULL,
    candidate_latency_ms DOUBLE PRECISION NOT NULL,
    stable_body          TEXT NOT NULL DEFAULT '',
    candidate_body       TEXT NOT NULL DEFAULT '',

    status_match         BOOLEAN NOT NULL,
    body_match           BOOLEAN NOT NULL,
    candidate_slow       BOOLEAN NOT NULL DEFAULT FALSE,
    outcome              TEXT NOT NULL CHECK (outcome IN ('match', 'different', 'error')),
    differences          TEXT[] NOT NULL DEFAULT '{}',
    candidate_error      TEXT,

    -- When the gateway received the request, not when the row was inserted.
    -- A timed out candidate can finish seconds later than the request.
    received_at          TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_comparisons_received_at ON request_comparisons (received_at DESC);
CREATE INDEX IF NOT EXISTS idx_comparisons_outcome ON request_comparisons (outcome);
CREATE INDEX IF NOT EXISTS idx_comparisons_trace_id ON request_comparisons (trace_id);
