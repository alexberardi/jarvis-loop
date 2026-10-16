-- The head land sent back to build for red CI (docs/decisions/2026-10-15-ci-red-unchanged-head.md):
-- a build that reports pr_opened with that same head goes to the user instead of review.
ALTER TABLE items ADD COLUMN ci_red_sha TEXT NOT NULL DEFAULT '';
