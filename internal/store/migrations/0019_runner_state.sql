-- A runner's state (docs/decisions/2026-10-18-runner-reload-drain.md): 'draining' while it claims
-- nothing new and waits for its live runs before a restart; '' otherwise (its next claim clears it).
ALTER TABLE runners ADD COLUMN state TEXT NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN state_at INTEGER NOT NULL DEFAULT 0;
