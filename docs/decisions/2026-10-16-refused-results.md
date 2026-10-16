# Refused results: skip tool state, retry once, say it is a runner error

2026-10-16 · item #29. Seen on #26: the review approved, but a tool it ran had left its own state
in `.loop/evidence/.claude/`; the runner refused the whole result (`evidence .claude is not a
regular file`) and the item went to needs-you worded as a question for the user.

- **Evidence** (`runner.EvidenceFiles`): directories and dotfiles in `.loop/evidence/` are skipped
  (logged as a warning, not uploaded, and they take no slot of the 20), unless the result's
  `evidence[]` names one: then the result is refused, as the agent meant it as evidence. Still
  refused as before: symlinks (to a file or a dir, dotfile or not), any other irregular entry, a
  file over 20 MB, more than 20 files, and an evidence folder that is a symlink or not a folder.
  The runner's own verify steps skip them the same way, with a warning naming the step.
  Amends the evidence line of `2026-10-15-runner-choices.md`.
- **One retry**: when the runner refuses a stage's result (validation, or its evidence) before it
  has pushed, posted or uploaded anything, it runs the agent once more in the same run, with the
  same brief plus a closing "Your previous result was refused" section naming the reason. The
  retry gets what is left of the run's time cap and is skipped when less than 5 minutes is left or
  the runner is stopping. The run's advance carries `result_retried: true`. The refused attempt's
  `stream.jsonl`, `stderr.log` and `brief.md` are kept in the run dir as `stream-1.jsonl`,
  `stderr-1.log` and `brief-1.md`. The run's numbers (advance metrics, PR footer, review comment)
  add both attempts' cost, turns, duration and token usage (the cache ratio from the sums);
  `session_id` is the retry's, `first_session_id` the refused one's. Not retried: no
  `result.json` at all, a non-zero exit, git, GitHub, push or upload failures, verify failures.
- **Wording**: a second refusal fails the item with `the <stage>'s result couldn't be accepted:
  <reason> (again, after one retry)`, and the bot's needs-you message for any `failed` run ends
  `Reply "retry" to run it again, or drop.` instead of `Reply in this thread to answer.` Any reply
  already returns the item to the stage that failed, so no new command was needed; loopd's stage
  rules are unchanged.
