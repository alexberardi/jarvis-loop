-- Regression runs (docs/decisions/2026-10-18-regression-trigger.md): the approver asks for the
-- end-to-end regression from Slack or the board, a runner runs jarvis-server's scripts/e2e and
-- reports it. One run is queued or running at a time; a running one holds its lease in its row.
CREATE TABLE regressions (
    id           INTEGER PRIMARY KEY,
    suite        TEXT    NOT NULL,                 -- web, mobile, all
    status       TEXT    NOT NULL,                 -- queued, running, passed, failed, error
    requested_by TEXT    NOT NULL,                 -- the actor (slack:U…, user)
    via          TEXT    NOT NULL DEFAULT '',      -- slack, board, api
    runner       TEXT    NOT NULL DEFAULT '',
    agent        TEXT    NOT NULL DEFAULT '',
    lease_id     TEXT    NOT NULL DEFAULT '',
    claimed_at   INTEGER NOT NULL DEFAULT 0,
    expires_at   INTEGER NOT NULL DEFAULT 0,
    note         TEXT    NOT NULL DEFAULT '',
    activity     TEXT    NOT NULL DEFAULT '',
    error        TEXT    NOT NULL DEFAULT '',      -- why it could not run (status error)
    e2e_run      TEXT    NOT NULL DEFAULT '',      -- scripts/e2e's own run id
    server_head  TEXT    NOT NULL DEFAULT '',
    mobile_head  TEXT    NOT NULL DEFAULT '',
    passed       INTEGER NOT NULL DEFAULT 0,
    failed       INTEGER NOT NULL DEFAULT 0,
    skipped      INTEGER NOT NULL DEFAULT 0,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX regressions_one_open ON regressions (status) WHERE status IN ('queued', 'running');
CREATE INDEX regressions_lease ON regressions (lease_id) WHERE lease_id != '';

-- The tests of a finished run, as its report.json had them (n from 1, in the report's order).
CREATE TABLE regression_tests (
    run_id     INTEGER NOT NULL REFERENCES regressions (id),
    n          INTEGER NOT NULL,
    suite      TEXT    NOT NULL,
    title      TEXT    NOT NULL,
    file       TEXT    NOT NULL DEFAULT '',
    line       INTEGER NOT NULL DEFAULT 0,
    status     TEXT    NOT NULL,                   -- passed, failed, timedOut, skipped, interrupted
    error      TEXT    NOT NULL DEFAULT '',        -- the assertion
    ms         INTEGER NOT NULL DEFAULT 0,
    screenshot INTEGER NOT NULL DEFAULT 0,         -- its regression_files n (0: none)
    repo       TEXT    NOT NULL DEFAULT '',        -- where a failure is filed (owner/name)
    PRIMARY KEY (run_id, n)
);

-- The files a run uploaded: the failures' screenshots and the report itself.
CREATE TABLE regression_files (
    run_id       INTEGER NOT NULL REFERENCES regressions (id),
    n            INTEGER NOT NULL,
    name         TEXT    NOT NULL,
    path         TEXT    NOT NULL,                 -- relative to the data dir
    sha256       TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    content_type TEXT    NOT NULL,
    at           INTEGER NOT NULL,
    PRIMARY KEY (run_id, n)
);

-- The items 'File it' made, by test (suite: title): a failing test is filed once while its item
-- is open.
CREATE TABLE regression_items (
    item_id INTEGER PRIMARY KEY REFERENCES items (id),
    test    TEXT    NOT NULL,
    run_id  INTEGER NOT NULL,
    n       INTEGER NOT NULL
);
CREATE INDEX regression_items_test ON regression_items (test);
