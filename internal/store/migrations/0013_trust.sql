-- Verify's refusal of a trusted-file change and the approver's OK for that one commit
-- (docs/decisions/2026-10-16-trusted-files.md): JSON (pipeline.Trust), '' for none.
ALTER TABLE items ADD COLUMN trust TEXT NOT NULL DEFAULT '';
