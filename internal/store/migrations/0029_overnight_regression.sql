-- The overnight regression (#152, docs/decisions/2026-10-19-overnight-regression.md): a run
-- scheduled for tonight waits (status scheduled, at most one) for its set time (not_before) and a
-- quiet queue (quiet_since: since when nothing was claimable, 0 while something is); its report is
-- posted at report_at (the morning), and reported_at says it was. Flaky tests (failed, then passed
-- on the rerun) are counted apart; a failure's recording (a voice reply) is a file like its
-- screenshot. The control row's regression_nightly schedules one every night at regression_at.
ALTER TABLE regressions ADD COLUMN not_before  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE regressions ADD COLUMN quiet_since INTEGER NOT NULL DEFAULT 0;
ALTER TABLE regressions ADD COLUMN report_at   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE regressions ADD COLUMN reported_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE regressions ADD COLUMN flaky       INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX regressions_one_scheduled ON regressions (status) WHERE status = 'scheduled';
ALTER TABLE regression_tests ADD COLUMN recording INTEGER NOT NULL DEFAULT 0;
ALTER TABLE control ADD COLUMN regression_nightly INTEGER NOT NULL DEFAULT 0;
ALTER TABLE control ADD COLUMN regression_at      TEXT    NOT NULL DEFAULT '';
