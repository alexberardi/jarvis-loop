-- The QA lane (docs/decisions/2026-10-15-qa.md): an item marked needs_qa waits in qa after it
-- merges, for the user's check; a Reopen files a new item linked to it by reopen_of.
ALTER TABLE items ADD COLUMN needs_qa INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN reopen_of INTEGER NOT NULL DEFAULT 0;
