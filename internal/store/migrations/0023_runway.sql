-- The runway warning (#205, pipeline/runway.go): the runway (agent-hours) below which loopd warns
-- an away user (0: off), and whether it warned in the current dry spell (cleared once the runway is
-- back at the threshold).
ALTER TABLE control ADD COLUMN runway_warn_hours INTEGER NOT NULL DEFAULT 3;
ALTER TABLE orch_state ADD COLUMN runway_warned INTEGER NOT NULL DEFAULT 0;
CREATE INDEX events_at ON events (at);
