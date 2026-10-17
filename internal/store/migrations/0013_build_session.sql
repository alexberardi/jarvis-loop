-- The OpenCode session of a qwen item's last build, which its next build resumes
-- (docs/decisions/2026-10-16-qwen-builder.md); '' for none. items.builder now changes too: a qwen
-- item's second strike sets it to 'claude'.
ALTER TABLE items ADD COLUMN build_session TEXT NOT NULL DEFAULT '';
