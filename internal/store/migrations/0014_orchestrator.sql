-- The orchestrator (docs/phases/08-orchestrator.md, piece 1).
-- slack_messages: the approver's messages loopd's Slack layer saw, so an orchestrator action that
-- names one (X-Loop-On-Behalf) can be checked against it: who wrote it, and when.
-- slack_message_uses: each (message, action, target) acts once.
CREATE TABLE slack_messages (
    channel TEXT    NOT NULL,
    ts      TEXT    NOT NULL,
    user    TEXT    NOT NULL,
    text    TEXT    NOT NULL DEFAULT '',
    at      INTEGER NOT NULL,
    PRIMARY KEY (channel, ts)
);
CREATE TABLE slack_message_uses (
    channel TEXT    NOT NULL,
    ts      TEXT    NOT NULL,
    action  TEXT    NOT NULL,
    target  TEXT    NOT NULL,
    actor   TEXT    NOT NULL,
    at      INTEGER NOT NULL,
    PRIMARY KEY (channel, ts, action, target)
);
