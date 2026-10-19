-- One runway warning per real change (#227, pipeline.RunwayWarn): what the current dry spell's
-- warnings said (JSON pipeline.RunwayWarned; '' when there is no dry spell). It replaces
-- runway_warned, whose one-tick clear re-sent the same warning after a blip or a paused tick.
ALTER TABLE orch_state ADD COLUMN runway_last TEXT NOT NULL DEFAULT '';
