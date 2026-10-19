-- Usage awareness (docs/decisions/2026-10-18-usage-awareness.md): a ledger of every run's usage, the
-- limits the CLI reported, the user's budgets, and which builder a lease runs (a paced claim may
-- route a build to qwen for that run).
ALTER TABLE control ADD COLUMN budgets TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN builder TEXT NOT NULL DEFAULT '';

CREATE TABLE usage_runs (
    id             INTEGER PRIMARY KEY,
    at             INTEGER NOT NULL, -- the run's advance (unix ns)
    started        INTEGER NOT NULL, -- at minus the run's seconds
    item_id        INTEGER,
    stage          TEXT    NOT NULL DEFAULT '',
    provider       TEXT    NOT NULL,
    model          TEXT    NOT NULL DEFAULT '',
    input          INTEGER NOT NULL DEFAULT 0,
    output         INTEGER NOT NULL DEFAULT 0,
    cache_creation INTEGER NOT NULL DEFAULT 0,
    cache_read     INTEGER NOT NULL DEFAULT 0,
    cost_usd       REAL    NOT NULL DEFAULT 0,
    requests       INTEGER NOT NULL DEFAULT 0,
    warning        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX usage_runs_at ON usage_runs (provider, at);

CREATE TABLE usage_hits (
    id       INTEGER PRIMARY KEY,
    at       INTEGER NOT NULL,
    provider TEXT    NOT NULL,
    limit_window TEXT NOT NULL,
    reset_at INTEGER NOT NULL DEFAULT 0, -- 0: the CLI gave no reset we could read
    burn     INTEGER NOT NULL DEFAULT 0, -- the window's burn when it hit
    item_id  INTEGER,
    error    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX usage_hits_at ON usage_hits (provider, at);

-- The pace the last usage_pace event announced, so a claim compares without searching the events.
CREATE TABLE usage_pace (
    provider TEXT PRIMARY KEY,
    level    INTEGER NOT NULL DEFAULT 0,
    agents   INTEGER NOT NULL DEFAULT 0
);

-- The runs recorded before the ledger, from their advance events (the same fields).
INSERT INTO usage_runs (at, started, item_id, stage, provider, input, output, cache_creation, cache_read, cost_usd, requests)
SELECT at,
       at - CAST(COALESCE(json_extract(data, '$.run_seconds'), '0') AS INTEGER) * 1000000000,
       item_id, from_stage,
       CASE WHEN json_extract(data, '$.qwen_tokens_input') IS NOT NULL THEN 'qwen' ELSE 'claude' END,
       CAST(COALESCE(json_extract(data, '$.qwen_tokens_input'), json_extract(data, '$.usage_input'), '0') AS INTEGER),
       CAST(COALESCE(json_extract(data, '$.qwen_tokens_output'), json_extract(data, '$.usage_output'), '0') AS INTEGER),
       CAST(COALESCE(json_extract(data, '$.usage_cache_creation'), '0') AS INTEGER),
       CAST(COALESCE(json_extract(data, '$.usage_cache_read'), '0') AS INTEGER),
       CAST(COALESCE(json_extract(data, '$.total_cost_usd'), '0') AS REAL),
       CAST(COALESCE(json_extract(data, '$.qwen_requests'), '0') AS INTEGER)
FROM events
WHERE item_id IS NOT NULL AND json_valid(data)
  AND (json_extract(data, '$.usage_input') IS NOT NULL OR json_extract(data, '$.qwen_tokens_input') IS NOT NULL);
