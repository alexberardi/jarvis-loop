-- #plan in Slack (docs/phases/07-planning.md, "Slack"). plan_turns.slack_ts: the Slack ts of the
-- messages a turn carries (a JSON list, parallel to nothing: messages from the API have none), so
-- the bot can react to them while the turn runs. plans.slack_header: the bot's message heading the
-- plan's thread (its stage and the Drop button), updated as the plan moves; it is the thread's
-- parent itself when the bot started the thread.
ALTER TABLE plan_turns ADD COLUMN slack_ts TEXT NOT NULL DEFAULT '[]';
ALTER TABLE plans ADD COLUMN slack_header TEXT NOT NULL DEFAULT '';
CREATE INDEX plans_slack_thread ON plans (slack_channel, slack_ts) WHERE slack_ts <> '';
