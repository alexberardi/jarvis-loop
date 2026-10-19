# Gemini second opinion on code review: adversarial findings, judged by Claude, advisory first (2026-10-18)

Item #190 (the user, 2026-10-17). The builder and the reviewer are both Claude: they share blind
spots. A reviewer from a different model family should catch some of what they both miss. The user
has Gemini through a free plan: only Gemini 3.8 Flash, limits unknown, through the Antigravity CLI
(`agy`, logged in on the dev box).

## Decision

**When.** After a review run approves (never on a bounce, never for triage or build), when the
runner config has a `gemini` block. Absent: off, no agy call.

```json
"gemini": {"agy": "~/.local/bin/agy", "model": "gemini-3.8-flash-high", "blocking": false, "timeout": "10m"}
```

Defaults: `agy` on PATH, `gemini-3.8-flash-high`, advisory, 10 minutes. `home` (default `~/.gemini`)
is agy's login and state; `fence` (default true) runs agy under the runner's fence.

**The Gemini pass** (`runner/gemini.go`). One headless call, in the PR's worktree:
`agy -p <prompt> --model <m> --mode plan --sandbox --output-format json --json-schema <file>`. The
prompt carries the item's ask, Claude's review and the diff inline (cut at 80 KiB, so it fits one
argv string; agy may skip the git-ignored `.loop/`), and asks for bugs, missing or weak tests,
security issues and gaps between the ask and the code. The schema: `findings`, each with `severity`
(high, medium, low), `file`, `line`, `claim`, `evidence`. The runner keeps at most 20, numbered.

**Unavailable never blocks.** Quota and rate limits, a timeout, a non-zero exit, a status of error,
output that is not the schema's JSON, a binary that will not start, or too little of the run's time
cap left: all are one outcome, `unavailable`, with a short reason (`quota`, `timeout`, `error`,
`bad_output`, `no_start`, `no_time`). The review's approval stands, no PR comment, and the
second_opinion event and the thread say so. agy's stdout and stderr stay in the runner's run dir
(`gemini-stdout.json`, `gemini-stderr.log`): they go to no comment, event or evidence.

**No slot held waiting on quota.** The pass runs inside the review's lease (it heartbeats it) but
never waits for quota: a quota error is `unavailable` at once, with no retry, and a hung call ends at
`gemini.timeout`. The pass and the adjudication fit in the run's own time cap (loopd's
`max_run_minutes`): agy gets at most what is left minus five minutes for the adjudication.

**Claude adjudicates** (`runner/secondopinion.go`). With findings, a second Claude turn runs in the
same worktree with the review's settings and guard (a fresh session, not the reviewer's: it should
not defend its own approval). Its brief (stage `adjudicate`) points at `.loop/context/gemini-findings.json`,
`diff.patch` and `ask.md`; it writes `{"verdicts": [{"id", "verdict": agree|disagree, "reason"}]}`,
one per finding, checked like any result. At most 10 minutes. If it gives no usable verdicts, the
second opinion is `unjudged`: recorded and posted, never blocking. The review's own transcript,
stderr and brief are kept as `stream-review.jsonl`, `stderr-review.log` and `brief-review.md`.

**Recorded.** One PR comment, `## Second opinion (Gemini)`: every finding with Claude's verdict
and reason, and what happened (advisory, sent back, already sent back). The findings and verdicts
as evidence, `gemini-findings.json`. A runner event, `second_opinion`, with counts only (status,
reason, head, model, findings and agreed in total and per severity, judged, blocking, bounced,
agy's usage object, the adjudication's cost): the Slack thread gets one line from it, and the board
adds the events up per item (card label, item page) and in total (the board's header), so after a
week the user can see the agreed rate by severity and decide on blocking. The approved or bounce
outcome carries `second_opinion` (its status, or `bounce`).

**Advisory first, blocking later.** For the first week (the user's choice) `blocking` is false: an
agreed high finding is recorded, nothing bounces. With `gemini.blocking: true` the runner reports a
`bounce` instead of `approved`, with the agreed high findings as the notes; loopd counts it as a
review bounce (bounce count, the bounce limit to needs-you, the orchestrator's review-limit answer),
exactly as a review agent's bounce.

**Never twice for one head.** Before a blocking bounce the runner reads the item's events: if a
second_opinion event already bounced this head, it records `repeat` and the approval stands. A build
that changes nothing cannot be sent back again by the same findings; a new head gets a new second
opinion. If loopd cannot say, or the event cannot be recorded, it does not bounce.

**The login stays agy's.** The runner never reads `~/.gemini`. It is added to the config's own
secrets: masked from every fenced command (setup, verify, the qwen builder) and unreadable to every
Claude agent. agy's own fence binds it writable again (it refreshes its tokens there), keeps the
worktree read-only and masks the other credential stores.

## Why

- A different model family is the point: the second reviewer is told to be adversarial, and the
  judge is Claude so that a noisy Flash model cannot send work back on its own.
- Advisory first because Flash's precision on this code is unknown; the board's agreed rate by
  severity is the evidence for turning blocking on.
- One failure outcome because a free plan's limits are unknown: the loop must never stall or fail
  an item on them.

## Not decided here

- Gemini on plan blueprints (#191) reuses the `gemini` block and `runAgy`.
- Usage awareness (#199) may move agy's usage from the event into its counters.
- Whether agy's own `--sandbox` nests under bubblewrap on the dev box was not tried with the real
  agy; if it does not, set `"fence": false` (agy then runs with the runner's environment minus
  loopd's and Slack's secrets, in plan mode).
