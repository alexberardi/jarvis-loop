-- Land's review_pending (docs/decisions/2026-10-24-land-waits-for-github-review.md): a land item
-- whose PR waits for an approving GitHub review its base requires is claimed again at not_before;
-- review_pending_sha and review_pending_since are the head that waits and since when (unix nanos).
ALTER TABLE items ADD COLUMN review_pending_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN review_pending_since INTEGER NOT NULL DEFAULT 0;
