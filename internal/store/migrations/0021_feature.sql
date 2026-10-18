-- An item's feature: the name that groups the small items one feature is built from ('' for none)
-- (docs/decisions/2026-10-18-feature-tags.md).
ALTER TABLE items ADD COLUMN feature TEXT NOT NULL DEFAULT '';
