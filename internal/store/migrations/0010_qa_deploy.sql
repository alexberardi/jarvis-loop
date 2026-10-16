-- QA after deploy (docs/decisions/2026-10-16-qa-after-deploy.md): an item of the loop's own repo
-- waits in qa for a deploy that contains its merge before the user is asked to check it.
ALTER TABLE items ADD COLUMN awaiting_deploy INTEGER NOT NULL DEFAULT 0;
