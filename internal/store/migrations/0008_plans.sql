-- Plans (docs/phases/07-planning.md, "Plans"): an idea talked into a brief and a blueprint, then
-- split into items. Times are Unix nanoseconds, UTC.

CREATE TABLE plans (
    id                  INTEGER PRIMARY KEY,
    title               TEXT    NOT NULL,
    stage               TEXT    NOT NULL,             -- brief, blueprint, split, done, dropped
    priority            TEXT    NOT NULL,
    repos               TEXT    NOT NULL DEFAULT '',  -- comma-separated slugs, like resources
    slack_channel       TEXT    NOT NULL DEFAULT '',
    slack_ts            TEXT    NOT NULL DEFAULT '',
    product_session     TEXT    NOT NULL DEFAULT '',
    engineering_session TEXT    NOT NULL DEFAULT '',
    brief_approved      INTEGER NOT NULL DEFAULT 0,   -- the approved version, 0: none
    blueprint_approved  INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);

-- Append-only, like events: each agent turn that changes a document adds a version.
CREATE TABLE plan_docs (
    plan_id    INTEGER NOT NULL REFERENCES plans (id),
    kind       TEXT    NOT NULL,                      -- brief, blueprint
    version    INTEGER NOT NULL,
    markdown   TEXT    NOT NULL,
    sha256     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (plan_id, kind, version)
);
CREATE TRIGGER plan_docs_no_update BEFORE UPDATE ON plan_docs
BEGIN
    SELECT RAISE(ABORT, 'plan documents are append-only');
END;
CREATE TRIGGER plan_docs_no_delete BEFORE DELETE ON plan_docs
BEGIN
    SELECT RAISE(ABORT, 'plan documents are append-only');
END;

-- One agent turn of a plan's conversation.
CREATE TABLE plan_turns (
    id            INTEGER PRIMARY KEY,
    plan_id       INTEGER NOT NULL REFERENCES plans (id),
    persona       TEXT    NOT NULL DEFAULT '',        -- product, engineering; set at claim
    messages      TEXT    NOT NULL DEFAULT '[]',      -- JSON list: the user's messages, verbatim
    back_to_brief TEXT    NOT NULL DEFAULT '',        -- why engineering sent the plan back
    status        TEXT    NOT NULL,                   -- queued, running, done, failed
    runner        TEXT    NOT NULL DEFAULT '',
    agent         TEXT    NOT NULL DEFAULT '',
    reply         TEXT    NOT NULL DEFAULT '',
    ready         INTEGER NOT NULL DEFAULT 0,
    error         TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX plan_turns_plan ON plan_turns (plan_id, id);
-- At most one queued and one running turn per plan: a message that arrives while a turn runs
-- joins the next queued turn.
CREATE UNIQUE INDEX plan_turns_one_queued ON plan_turns (plan_id) WHERE status = 'queued';
CREATE UNIQUE INDEX plan_turns_one_running ON plan_turns (plan_id) WHERE status = 'running';

-- A running turn's lease, as leases is an item's (leases.item_id must name an item). Lease ids
-- are one namespace: POST /leases/{id}/heartbeat finds either.
CREATE TABLE plan_leases (
    id         TEXT    PRIMARY KEY,
    turn_id    INTEGER NOT NULL REFERENCES plan_turns (id),
    plan_id    INTEGER NOT NULL REFERENCES plans (id),
    runner     TEXT    NOT NULL,
    agent      TEXT    NOT NULL,
    claimed_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    note       TEXT    NOT NULL DEFAULT '',
    ended_at   INTEGER,                               -- NULL while live
    end_reason TEXT    NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX plan_leases_one_live ON plan_leases (turn_id) WHERE ended_at IS NULL;

-- A plan's history goes in events too, with item_id NULL.
ALTER TABLE events ADD COLUMN plan_id INTEGER;
CREATE INDEX events_plan ON events (plan_id, id) WHERE plan_id IS NOT NULL;
