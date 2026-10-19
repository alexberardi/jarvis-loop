-- Done proofs (docs/decisions/2026-10-18-done-proofs.md): a done item's verification status,
-- separate from its stage ('' on an item not done), the line that says why, and when the done sweep
-- asked the orchestrator for its proof (unix ns; 0: not asked).
ALTER TABLE items ADD COLUMN verify TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN verify_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN verify_asked INTEGER NOT NULL DEFAULT 0;

-- The done items so far: verified when their check in qa passed (the user's or the proof's),
-- failed when it reopened them, else unproven (merged and main green is all "done" said).
UPDATE items SET verify = 'unproven' WHERE stage = 'done';
UPDATE items SET verify = 'verified', verify_reason = 'verified in qa'
WHERE stage = 'done' AND EXISTS (SELECT 1 FROM events e WHERE e.item_id = items.id AND e.kind = 'verified');
UPDATE items SET verify = 'failed',
       verify_reason = 'reopened in qa as #' || (SELECT MAX(r.id) FROM items r WHERE r.reopen_of = items.id)
WHERE stage = 'done' AND EXISTS (SELECT 1 FROM events e WHERE e.item_id = items.id AND e.kind = 'reopened');
CREATE INDEX items_verify ON items (stage, verify);
