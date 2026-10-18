-- Main's green head when land, at a red CI, saw main red at the PR's base and green now
-- (docs/decisions/2026-10-17-needs-you-rebuild.md): a build that changes nothing is then asked to
-- rebuild (rebase on main), not to rerun CI.
ALTER TABLE items ADD COLUMN ci_main_fixed TEXT NOT NULL DEFAULT '';
