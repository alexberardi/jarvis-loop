-- Item dependencies and the plan link (docs/phases/07-planning.md): depends_on is the ids of the
-- items this one waits for, comma-separated like resources; plan_id and plan_piece link an item
-- filed from a plan to its piece (0: none; the plans table comes with plans themselves).
ALTER TABLE items ADD COLUMN depends_on TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN plan_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN plan_piece INTEGER NOT NULL DEFAULT 0;
