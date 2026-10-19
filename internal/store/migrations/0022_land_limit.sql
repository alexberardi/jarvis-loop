-- Land runs no agent and mostly waits on CI (docs/decisions/2026-10-18-land-never-waits.md): its
-- default limit goes from 1 to 3. A control row still at the old default of 1 moves to 3; a limit
-- the user set to anything else stays.
UPDATE control SET limits = json_set(limits, '$.land', 3)
WHERE json_extract(limits, '$.land') = 1;
