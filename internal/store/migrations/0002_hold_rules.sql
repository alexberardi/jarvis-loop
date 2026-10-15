-- Hold risk computed by loopd (docs/decisions/2026-10-14-hold-rules.md).
-- items.hold_reason: why an item is on the hold list ("auth: M internal/modules/auth/x.go; ...").
-- control.hold_rules: JSON list of {name, glob, statuses}; '' means the built-in defaults
-- (pipeline.DefaultHoldRules), so the defaults live in one place until the user sets their own.
ALTER TABLE items ADD COLUMN hold_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE control ADD COLUMN hold_rules TEXT NOT NULL DEFAULT '';
