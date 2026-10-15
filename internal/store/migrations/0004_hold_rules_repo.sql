-- Per-repo hold rules (docs/phases/06-land.md): a rule may name the repo (owner/name) it applies
-- to. Rules a user set before this were jarvis-server's hold list, so each one without a repo is
-- tagged with jarvis-server's slug. '' (the built-in defaults, which now carry their repos) is left
-- alone.
UPDATE control SET hold_rules = (
    SELECT json_group_array(
        CASE WHEN json_type(value, '$.repo') IS NULL OR json_extract(value, '$.repo') = ''
             THEN json_set(value, '$.repo', 'alexberardi/jarvis-server')
             ELSE json(value) END)
    FROM json_each(control.hold_rules))
WHERE hold_rules <> '';
