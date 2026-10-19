-- Land's ci_pending (docs/decisions/2026-10-19-ci-pending-requeue.md): a land item put back while
-- its PR's CI is still pending is claimed again at not_before (unix nanos, 0: now); ci_pending_since
-- and ci_pending_sha are when CI started pending on that head, across land runs.
ALTER TABLE items ADD COLUMN not_before INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN ci_pending_since INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN ci_pending_sha TEXT NOT NULL DEFAULT '';
