-- A repo whose main CI is red is frozen until it is green (docs/decisions/2026-10-18-main-red-freeze.md):
-- item_id is its main-red P1, the one item that still moves; red_since is when the culprit merged.
CREATE TABLE repo_freezes (
	id INTEGER PRIMARY KEY,
	repo TEXT NOT NULL,
	item_id INTEGER NOT NULL REFERENCES items(id),
	since INTEGER NOT NULL,
	red_since INTEGER NOT NULL,
	jobs TEXT NOT NULL DEFAULT '',
	lifted_at INTEGER,
	lifted_by TEXT NOT NULL DEFAULT '',
	lift_reason TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX repo_freezes_live ON repo_freezes (repo) WHERE lifted_at IS NULL;

-- A run that finished in a frozen repo: its advance, kept to apply once main is green, in the
-- order the runs finished (id).
CREATE TABLE queued_advances (
	id INTEGER PRIMARY KEY,
	freeze_id INTEGER NOT NULL REFERENCES repo_freezes(id),
	item_id INTEGER NOT NULL REFERENCES items(id),
	lease_id TEXT NOT NULL,
	runner TEXT NOT NULL,
	agent TEXT NOT NULL,
	stage TEXT NOT NULL,
	outcome TEXT NOT NULL,
	fields TEXT NOT NULL,
	changed_paths TEXT NOT NULL,
	queued_at INTEGER NOT NULL,
	applied_at INTEGER
);
CREATE INDEX queued_advances_pending ON queued_advances (item_id) WHERE applied_at IS NULL;
