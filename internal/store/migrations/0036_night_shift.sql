-- The night shift (docs/decisions/2026-10-20-night-shift.md): its mode (off, on, auto) and the
-- classes the user turned off (a JSON list; '': none) on the control row; the user's last
-- "@orchestrator night" / "morning" and when (unix ns) on the orchestrator's state.
ALTER TABLE control ADD COLUMN night_shift TEXT NOT NULL DEFAULT 'off';
ALTER TABLE control ADD COLUMN night_classes_off TEXT NOT NULL DEFAULT '';
ALTER TABLE orch_state ADD COLUMN night_override TEXT NOT NULL DEFAULT '';
ALTER TABLE orch_state ADD COLUMN night_override_at INTEGER NOT NULL DEFAULT 0;
