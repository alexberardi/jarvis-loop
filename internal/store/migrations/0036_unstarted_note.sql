-- When the note that a scheduled overnight run had still not started at 07:00 was posted in #loop
-- (unix ns; 0: not posted), so it is posted once per run (#317).
ALTER TABLE regressions ADD COLUMN unstarted_noted_at INTEGER NOT NULL DEFAULT 0;
