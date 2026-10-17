-- Orchestrate turns (docs/phases/08-orchestrator.md, piece 3;
-- docs/decisions/2026-10-17-orchestrate-turns.md).

-- The daily cap of turns (0: the orchestrator is off).
ALTER TABLE control ADD COLUMN orchestrator_turns_per_day INTEGER NOT NULL DEFAULT 150;

-- One orchestrator turn. A queued turn collects its triggers; what arrives while one runs joins
-- the next queued one. A running turn holds its lease in its own row (one turn runs at a time).
CREATE TABLE orch_turns (
    id          INTEGER PRIMARY KEY,
    status      TEXT    NOT NULL,                 -- queued, running, done, failed
    triggers    TEXT    NOT NULL DEFAULT '[]',    -- JSON list of pipeline.Trigger
    day         TEXT    NOT NULL DEFAULT '',      -- the day it started on (the cap, the session)
    resume      TEXT    NOT NULL DEFAULT '',      -- the session it resumed ('' for a new one)
    session     TEXT    NOT NULL DEFAULT '',      -- the session it ended with
    fingerprint TEXT    NOT NULL DEFAULT '',      -- the board it saw (the sweep compares it)
    actions     TEXT    NOT NULL DEFAULT '[]',    -- JSON: how each action it asked for went
    error       TEXT    NOT NULL DEFAULT '',
    runner      TEXT    NOT NULL DEFAULT '',
    agent       TEXT    NOT NULL DEFAULT '',
    lease_id    TEXT    NOT NULL DEFAULT '',
    claimed_at  INTEGER NOT NULL DEFAULT 0,
    expires_at  INTEGER NOT NULL DEFAULT 0,
    note        TEXT    NOT NULL DEFAULT '',
    activity    TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX orch_turns_one_queued ON orch_turns (status) WHERE status = 'queued';
CREATE UNIQUE INDEX orch_turns_one_running ON orch_turns (status) WHERE status = 'running';
CREATE INDEX orch_turns_day ON orch_turns (day) WHERE day != '';
CREATE INDEX orch_turns_lease ON orch_turns (lease_id) WHERE lease_id != '';

-- The orchestrator's state: its session and the day it started (rotated daily), the last sweep,
-- when a wait it was told about ends (due_at, 0: none), and the day the cap was last reported.
CREATE TABLE orch_state (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    session     TEXT    NOT NULL DEFAULT '',
    session_day TEXT    NOT NULL DEFAULT '',
    last_sweep  INTEGER NOT NULL DEFAULT 0,
    due_at      INTEGER NOT NULL DEFAULT 0,
    capped_day  TEXT    NOT NULL DEFAULT ''
);
INSERT INTO orch_state (id) VALUES (1);

-- The items an idle trigger already went out for, at the updated_at it saw: one trigger per wait.
CREATE TABLE orch_idle (
    item_id    INTEGER PRIMARY KEY REFERENCES items (id),
    updated_at INTEGER NOT NULL
);
