-- The test Pi (docs/decisions/2026-10-18-pi-device.md): one item at a time. An install that
-- already set its own "pi" limit keeps it.
UPDATE control SET limits = json_set(limits, '$.pi', 1) WHERE id = 1 AND json_extract(limits, '$.pi') IS NULL;
