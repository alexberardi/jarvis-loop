-- Slack (docs/phases/03-slack.md).
-- items.slack_channel / slack_ts: the item's parent message in #loop; its thread holds the history.
-- control.slack_control_ts: the pinned active-agents message in #loop-digest, so a restart updates
-- it instead of posting another. control.last_digest_at: the digest window last reported (Unix ns),
-- so a restart never posts the same digest twice.
ALTER TABLE items ADD COLUMN slack_channel TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN slack_ts TEXT NOT NULL DEFAULT '';
CREATE INDEX items_slack_thread ON items (slack_channel, slack_ts) WHERE slack_ts <> '';
ALTER TABLE control ADD COLUMN slack_control_ts TEXT NOT NULL DEFAULT '';
ALTER TABLE control ADD COLUMN last_digest_at INTEGER NOT NULL DEFAULT 0;
