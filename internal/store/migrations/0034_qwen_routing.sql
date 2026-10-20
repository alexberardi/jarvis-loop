-- The Qwen builder's routing (docs/decisions/2026-10-19-qwen-routing.md): who built the PR's head,
-- the item's failed qwen builds, and what kind of change triage says it is ('' when not said).
ALTER TABLE items ADD COLUMN built_by TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN qwen_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN change_kind TEXT NOT NULL DEFAULT '';
-- At most one qwen build at a time across runners (prod's background label): a control limit
-- like gpu's, added to a control row that has none.
UPDATE control SET limits = json_set(limits, '$.qwen', 1)
WHERE json_extract(limits, '$.qwen') IS NULL;
