-- The repos each runner has a checkout of (owner/name, comma-separated, sorted; '': it did not
-- say, so any), sent with every claim: a claim gets only items on them, and an item on a repo no
-- runner seen lately has is flagged unservable (#467).
ALTER TABLE runners ADD COLUMN repos TEXT NOT NULL DEFAULT '';
