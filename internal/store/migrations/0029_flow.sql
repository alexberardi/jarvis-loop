-- The flow panel (docs/decisions/2026-10-18-flow-panel.md): its 6h/24h/7d windows read the leases
-- that ended in the window, which were full scans (events by time use 0023_runway's events_at); and
-- a runner reports how many slots it works, so idle capacity can be shown (0: a runner that
-- predates it, unknown).
CREATE INDEX leases_ended ON leases (ended_at);
CREATE INDEX plan_leases_ended ON plan_leases (ended_at);
ALTER TABLE runners ADD COLUMN slots INTEGER NOT NULL DEFAULT 0;
