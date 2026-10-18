# A run that ends with no result resumes its session once (2026-10-18)

Item #208. Nineteen build and review runs (#37 … #194, and #161's build after #200 merged) exited
with code 0 and no `.loop/result.json`. Their last words were "The race suite is still running.
I'll wait for it before writing the result." and the like: the agent had started a check in the
background (`run_in_background`, Monitor) and ended its turn to wait for it. `claude -p` is one
shot: it ends when the model stops, and a background task never reports back. The runner failed
each one to the user; #200's resume covers interrupted Claude builds only, not a clean exit.

## Decision

- **The briefs say so** (`skills/build`, `skills/review`, `skills/triage`): a "Run everything in
  the foreground" section: never `run_in_background`, Monitor or a trailing `&`, never end a turn
  saying you will wait; raise the Bash `timeout` (up to 600000 ms) or split a long check.
- **The runner resumes once** (`runner.work`): a triage, build or review run that exits 0 with a
  session id and no `.loop/result.json` (the file is missing, not refused) runs again in the same
  claim, resuming the same session (`--resume <session>`, or `-s` for qwen) with a short note for a
  prompt (`runner.NoResultNote`, heading `# Your run ended without a result`): what happened, that
  the background process is gone, re-run it in the foreground and write the result. The worktree
  and its `.loop/brief.md` stay as they were.
- **It is the stage's one retry**, as in `2026-10-16-refused-results.md`: same time rule (skipped
  when less than 5 minutes of the cap is left or the runner is stopping), same kept files
  (`stream-1.jsonl`, `stderr-1.log`, `brief-1.md`), same summed numbers and `first_session_id`. A
  run whose refused result was already retried is not resumed, and a resumed run whose result is
  refused fails at once. The advance carries `result_resumed: true` (and `result_retried: true`).
- **Still nothing**: the item fails to the user with `the agent exited (code 0) after its session
  was resumed once, still without a usable .loop/result.json (…)`. When the resume is skipped it
  says `the agent exited (code 0) without writing .loop/result.json`.

Not done: detecting the background call in the stream (the resume does not need it), and an env
var to disable Claude Code's background tasks (not checked that one exists).
