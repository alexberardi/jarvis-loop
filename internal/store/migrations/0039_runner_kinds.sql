-- The kinds of work each runner last claimed (item, plan, orchestrate, regression; comma-separated,
-- sorted; '': items only), so the overnight run's 07:00 note can say no runner takes it (#416).
-- unstarted_noted_at (0037) now holds the latest note, one a night, not one a run.
ALTER TABLE runners ADD COLUMN kinds TEXT NOT NULL DEFAULT '';
