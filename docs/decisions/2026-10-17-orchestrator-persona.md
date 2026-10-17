# The orchestrator persona and its recorded-situation scenarios (2026-10-17)

Item #74, piece 4 of [phase 8](../phases/08-orchestrator.md). Pieces 1 and 3 gave the orchestrator
its authority (loopd's routes and limits) and its turns; this is who it is and what it does with
them.

## Decided

- **The persona is a skill**, `skills/orchestrator/SKILL.md`, in the house style of the other
  skills: where it is, the authority table and the Never list, the answer kinds, what stays with
  the user, how to escalate (`text` plus a `recommendation` with a because, one decision each),
  the process-issue patterns and how to file a P1 ticket against `alexberardi/jarvis-loop` (a
  `signature` loopd deduplicates on, the items and a few log lines in `ask`, a note; agents down
  plus an escalation when every run fails), and notebook habits.
- **The spec lists six answer kinds; the contract has five.** Kind 3, a runner or environment
  error, is a `retry` (loopd refuses to answer a run failure), and the skill says so.
- **Red CI is two `red_ci` answers, not one plus a bounce.** The first ("land reads CI again")
  returns the item to land. If CI is red again on the same head, land's own back-to-build is the
  second bounce, so the item comes to the orchestrator at the bounce limit with the log attached;
  the second `red_ci` answer sends it to build. A third red on that head is escalated.
- **The prompt**: `runner.OrchStatic` is the skill (without its frontmatter) followed by the
  runner's result contract, which stays in the runner next to the code that parses it. A golden
  file (`internal/runner/testdata/brief_orchestrate.golden.md`) pins it, as for the stages.
- **The snapshot's items carry their `resources`**, so "ready but unclaimed" can be told from the
  runners' capabilities. Runners still have no slot count in the snapshot; nothing here needed it.
- **Scenarios: one `persona` play, not one play per situation.** The fake claude's `persona` play
  (`internal/fakeclaude/persona.go`) applies the skill's rules to `.loop/context/snapshot.json`:
  retry run failures and note their normalised signature (`pipeline.NormaliseSignature`), file a
  ticket once a signature is on two items (this turn's or the notebook's), retry what the notebook
  waits on when a `due` turn starts, answer the kinds, escalate the second bounce limit, a re-asked
  question it already answered, and a third red CI, and file a ticket for an item idle an hour that
  no runner can serve. One play shows the rules compose; the six scenarios
  (`scenarios/59-orchestrator-*.json`) are the situations of 2026-10-16 built from real loopd state
  (new build plays `lock_error` and `harness_question`; the `triage` step takes `resources`).
- **What they test, and what not.** The fake does not read the skill, so the scenarios check how
  the loop carries out the actions the persona prescribes (loopd's limits, refusals, dedup, events,
  the build seeing the answer), not the model's judgment. A check of the model itself would be an
  opt-in eval running the real `claude -p` on recorded snapshots, outside CI: a follow-up.
