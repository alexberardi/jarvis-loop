-- The waiting stage (docs/decisions/2026-10-22-waiting-stage.md): the PRs a parked item waits on
-- besides its dependencies (JSON: [{"ref": "owner/name#444", "merged": true}]; '': none), and when
-- it was parked (unix ns; 0: not waiting). The items it waits on are its depends_on.
ALTER TABLE items ADD COLUMN wait_prs TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN waiting_since INTEGER NOT NULL DEFAULT 0;
