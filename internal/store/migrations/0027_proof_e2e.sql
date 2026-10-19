-- E2E results decide e2e checks (#195, docs/decisions/2026-10-18-qa-proofs.md): the items in qa
-- whose proof the sweep wrote with only manual checks, before that, are asked once more
-- (proof_asked 2; the sweep's proof_e2e trigger, then 3): tests may now cover the change.
UPDATE items SET proof_asked = 2
WHERE stage = 'qa' AND proof != '' AND json_extract(proof, '$.by') = 'sweep'
  AND NOT EXISTS (SELECT 1 FROM json_each(proof, '$.checks') WHERE json_extract(value, '$.kind') != 'manual');
