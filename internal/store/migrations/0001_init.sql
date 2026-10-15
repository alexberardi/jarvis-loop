-- loopd's first schema (docs/phases/02-loopd-core.md). Times are Unix nanoseconds, UTC.

CREATE TABLE items (
    id            INTEGER PRIMARY KEY,
    title         TEXT    NOT NULL,
    ask           TEXT    NOT NULL DEFAULT '',
    repo          TEXT    NOT NULL DEFAULT '',
    source        TEXT    NOT NULL,
    priority      TEXT    NOT NULL,
    stage         TEXT    NOT NULL,
    return_stage  TEXT    NOT NULL DEFAULT '',
    question      TEXT    NOT NULL DEFAULT '',
    area          TEXT    NOT NULL DEFAULT '',
    size          TEXT    NOT NULL DEFAULT '',
    risk          TEXT    NOT NULL DEFAULT 'normal',
    resources     TEXT    NOT NULL DEFAULT '',      -- comma-separated, sorted
    bounce_count  INTEGER NOT NULL DEFAULT 0,
    pr            INTEGER NOT NULL DEFAULT 0,
    branch        TEXT    NOT NULL DEFAULT '',
    head_sha      TEXT    NOT NULL DEFAULT '',
    builder_agent TEXT    NOT NULL DEFAULT '',
    merge_sha     TEXT    NOT NULL DEFAULT '',
    summary       TEXT    NOT NULL DEFAULT '',
    approval      TEXT    NOT NULL DEFAULT '',      -- JSON, empty when none
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX items_stage ON items (stage);

-- Append-only: the triggers below refuse every UPDATE and DELETE.
CREATE TABLE events (
    id         INTEGER PRIMARY KEY,
    item_id    INTEGER,                              -- NULL for control changes
    at         INTEGER NOT NULL,
    actor      TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    from_stage TEXT    NOT NULL DEFAULT '',
    to_stage   TEXT    NOT NULL DEFAULT '',
    outcome    TEXT    NOT NULL DEFAULT '',
    data       TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX events_item ON events (item_id, id);
CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TABLE leases (
    id         TEXT    PRIMARY KEY,
    item_id    INTEGER NOT NULL REFERENCES items (id),
    stage      TEXT    NOT NULL,
    runner     TEXT    NOT NULL,
    agent      TEXT    NOT NULL,
    resources  TEXT    NOT NULL DEFAULT '',
    claimed_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    note       TEXT    NOT NULL DEFAULT '',
    ended_at   INTEGER,                              -- NULL while live
    end_reason TEXT    NOT NULL DEFAULT ''
);
-- At most one live lease per item: the database itself refuses a double claim.
CREATE UNIQUE INDEX leases_one_live ON leases (item_id) WHERE ended_at IS NULL;

CREATE TABLE evidence (
    item_id      INTEGER NOT NULL REFERENCES items (id),
    n            INTEGER NOT NULL,
    lease_id     TEXT    NOT NULL,
    label        TEXT    NOT NULL DEFAULT '',
    name         TEXT    NOT NULL,
    path         TEXT    NOT NULL,                   -- relative to the data dir
    sha256       TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    content_type TEXT    NOT NULL,
    uploaded_by  TEXT    NOT NULL,
    at           INTEGER NOT NULL,
    PRIMARY KEY (item_id, n)
);
CREATE INDEX evidence_lease ON evidence (lease_id);

CREATE TABLE runners (
    name         TEXT    PRIMARY KEY,
    capabilities TEXT    NOT NULL DEFAULT '',
    last_seen    INTEGER NOT NULL
);

CREATE TABLE control (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    active_agents   INTEGER NOT NULL,
    stopped         INTEGER NOT NULL,
    max_run_minutes INTEGER NOT NULL,
    limits          TEXT    NOT NULL                 -- JSON object
);
INSERT INTO control (id, active_agents, stopped, max_run_minutes, limits)
VALUES (1, 4, 0, 45, '{"emulator":1,"gpu":2,"land":1,"review":2,"triage":1,"voice":1}');
