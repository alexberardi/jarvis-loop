-- #415: a failed done item whose newest reopen triage closed not reproducible (and not as a
-- duplicate) goes back to unproven, without its proof, so the done sweep asks for a new one. From
-- now on Advance does this as the reopen closes (store.reopenNotReproducible); this is the items
-- that failed before it, mostly on the proof's environment (no pytest, docker not found).
UPDATE items SET verify = 'unproven', proof = '', verify_asked = 0,
       verify_reason = substr('reopen #' || r.id || ' closed not reproducible: ' ||
           CASE WHEN instr(trim(r.summary), char(10)) > 0
                THEN substr(trim(r.summary), 1, instr(trim(r.summary), char(10)) - 1)
                ELSE trim(r.summary) END, 1, 400)
FROM (SELECT o.id AS orig, n.id, n.summary FROM items o
      JOIN items n ON n.id = (SELECT MAX(x.id) FROM items x WHERE x.reopen_of = o.id)
      WHERE o.stage = 'done' AND o.verify = 'failed' AND n.stage = 'closed'
        AND lower(trim(n.summary)) NOT LIKE 'duplicate%'
        AND EXISTS (SELECT 1 FROM events e WHERE e.item_id = n.id AND e.outcome = 'not_reproducible')) r
WHERE items.id = r.orig;
