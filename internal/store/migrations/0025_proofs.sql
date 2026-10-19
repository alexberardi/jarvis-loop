-- QA proofs (docs/decisions/2026-10-18-qa-proofs.md): how the loop can tell a merged item works
-- (pipeline.Proof as JSON; '' for none), and whether the proof sweep has asked the orchestrator to
-- write one for an item that reached qa without it (once per item).
ALTER TABLE items ADD COLUMN proof TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN proof_asked INTEGER NOT NULL DEFAULT 0;
