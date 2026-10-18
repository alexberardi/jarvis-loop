-- What each runner last claimed with, for the held slots (#192, pipeline/slothold.go): the only
-- resources it takes, how often it polls (0: it did not say, the runner's default of 30 s) and
-- the work stages it takes in all its lanes ('': it did not say, so any).
ALTER TABLE runners ADD COLUMN only_resources TEXT NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN poll_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners ADD COLUMN stages TEXT NOT NULL DEFAULT '';
