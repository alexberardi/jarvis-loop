-- What a running agent is doing (docs/decisions/2026-10-16-working-now.md): the runner's latest
-- heartbeat activity (pipeline.Activity as JSON), on the live lease only: '' once it ends.
ALTER TABLE leases ADD COLUMN activity TEXT NOT NULL DEFAULT '';
ALTER TABLE plan_leases ADD COLUMN activity TEXT NOT NULL DEFAULT '';
