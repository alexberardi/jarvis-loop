-- Unservable items (docs/decisions/2026-10-17-unservable-items.md): an item in a work stage whose
-- resources no runner seen lately offers. One row while it is flagged; the row goes when a runner
-- with them shows up (or the item leaves the work stages).
CREATE TABLE unservable (
    item_id INTEGER PRIMARY KEY REFERENCES items (id),
    missing TEXT    NOT NULL,   -- comma-joined, sorted: the resources no runner offers
    since   INTEGER NOT NULL
);
