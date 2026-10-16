-- Who builds an item filed from a plan (docs/decisions/2026-10-16-qwen-builder-plans.md):
-- 'qwen' or 'claude', from the blueprint piece's builder line; '' for items not from a plan.
-- loopd stores it and shows it (/loop show, the board card); nothing routes on it yet.
ALTER TABLE items ADD COLUMN builder TEXT NOT NULL DEFAULT '';
