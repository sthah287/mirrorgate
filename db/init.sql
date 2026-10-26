CREATE TABLE IF NOT EXISTS request_comparisons (
    id                   BIGSERIAL PRIMARY KEY,
    request_id           TEXT NOT NULL UNIQUE,
    -- W3C trace id, so a row can be looked up in Jaeger.
    trace_id             TEXT,
    -- Position of this request in the gateway's stream of eligible requests,
    -- counted from 1 when the gateway process started. This is what makes
    -- "requests until detection" an exact count instead of elapsed * rate.
    request_seq          BIGINT NOT NULL DEFAULT 0,
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
    -- Gateway clock.
    received_at          TIMESTAMPTZ NOT NULL,
    -- When the worker finished comparing and stored the row. Worker clock, so
    -- the difference from received_at crosses processes and is only as good
    -- as the agreement between the two container clocks.
    detected_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_comparisons_received_at ON request_comparisons (received_at DESC);
CREATE INDEX IF NOT EXISTS idx_comparisons_outcome ON request_comparisons (outcome);
CREATE INDEX IF NOT EXISTS idx_comparisons_trace_id ON request_comparisons (trace_id);

-- The gateway writes comparison events here instead of publishing straight to
-- Redpanda, and the relay moves them to the topic. A broker outage then delays
-- comparisons instead of losing them.
CREATE TABLE IF NOT EXISTS comparison_outbox (
    request_id   TEXT PRIMARY KEY,
    payload      JSONB NOT NULL,
    -- The W3C traceparent captured when the gateway wrote the row. The relay
    -- publishes later, when the original context is long gone, so the trace
    -- has to travel in the row to stay in one piece.
    traceparent  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT
);

-- The relay only ever scans for unpublished rows, oldest first.
CREATE INDEX IF NOT EXISTS idx_outbox_pending ON comparison_outbox (published_at, created_at);
