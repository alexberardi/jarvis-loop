# Usage awareness: a ledger, learned limits, budgets, and pacing at claim

2026-10-18 · item #199 (user: "would be huge"). The loop runs on the user's Claude Max plan (a rolling
5-hour window and a weekly cap), Qwen on prod (free, slower) and, later, Gemini. A run that hits a
limit in the middle wastes itself; until now loopd only reacted afterwards, by setting the
active-agents number to 0 and never raising it again.

## The ledger

- Each run's advance already carried `total_cost_usd` and the token counts (`usage_input`, `_output`,
  `_cache_creation`, `_cache_read`; qwen: `qwen_tokens_*`, `qwen_requests`). The runner now also
  sends `model` (the stream's init event) and the CLI's last `rate_limit_event` reading:
  `rate_limit_status` (`allowed`, `allowed_warning`, `rejected`), `rate_limit_type`
  (`five_hour`, `seven_day…`), `rate_limit_resets_at`, `rate_limit_utilization`. Tested on a recorded
  stream (`internal/runner/testdata/usage_stream.jsonl`).
- loopd writes one row per run into `usage_runs` (migration 0025: item, stage, provider, model,
  tokens, cost, the run's start and end, the warning). The migration backfills the rows from the
  runs recorded before it, from their events.
- `usage_hits` records every limit a run hit: the window (from `rateLimitType`, else the text:
  "weekly", "Opus limit", "Sonnet limit" are the week, the rest the 5h window), the reset (the
  event's `resetsAt`, else the CLI's text "3:45pm", "Mon 12:00am", optionally with an IANA zone in
  parentheses, read in America/New_York like the orchestrator's day), and the window's burn at that
  moment.

## The unit: burn

Cache reads dominate the raw counts (a typical build: 1.4M cache reads, 19k output), so raw tokens
would make every run look alike. **Burn** is input-token equivalents weighted as Anthropic prices
them: input 1, output 5, cache writes 1.25, cache reads 0.1. The ratios are the same on every
current Claude model, so burn is proportional to cost without depending on the price list or the
CLI's cost estimate. Qwen's burn is its plain tokens (it is never paced). Budgets are in burn
("2M"), and the board, the CLI and Slack say "burn" next to the number.

## The windows

The plan does not publish its caps or tell a runner when the 5h window started, and the loop sees
only its own runs (not the user's interactive sessions):

- **5h window:** opens when a claude run starts after the previous window ended, and lasts 5 hours;
  when a limit hit reported its reset, that window ends at the reset (anchored). A run counts in
  the window it ended in.
- **Week:** from the last weekly limit's reported reset, in 7-day steps; with none known, the last
  7 days, rolling.
- **Learned limit:** the window's burn when its last limit hit. It is the loop's share of the plan
  (the user's own sessions also count against the plan), so it is conservative in the direction
  that matters only when the user was idle.

## Budgets

`Control.Budgets` (stored in `control.budgets`; '' is the default), per provider (only `claude`):
per window, either burn (`per_5h`, `per_week`) or a percent of the learned limit (`per_5h_pct`,
`per_week_pct`), and `pace_at` (default 85). The default is 100% of the learned limit in both windows:
nothing paces until a limit has been hit once. Set with `loop budget claude --per-5h 2M --per-week 80%
--pace-at 90` (`off` clears a window), or `PUT /control {"budgets": …}`.

## Pacing: at claim, never in a run

`pipeline.PaceFor` turns the usage into a pace; `ChooseWorkPaced` applies it to the claim. Running
agents are never touched.

Since 2026-10-20 ([pacing never starves](2026-10-20-pacing-never-starves.md)): a slow pace with no
P1, size S or triage item ready still takes the first item while fewer than max(1, agents/3) claude
runs are live; `loop budget claude --warnings ignore` turns the CLI's warning off; and
`loop pace normal --until <time|reset>` lifts pacing until a time or the next limit hit.

- **Normal** below `pace_at` of both budgets.
- **Slow** from `pace_at` (or when a run in the open window got the CLI's `allowed_warning`): claude's
  runs at once are the active-agents number minus one at `pace_at`, minus one more each further
  step of (100 − pace_at)/(n − 1) percent, never below one (4 agents: 3 at 85%, 2 at 90%, 1 at 95%).
  Claude takes only P1 and size S items, triage (short, and the item has no size before it) and P1
  plan turns; the orchestrate turn still gets its slot. P3–P5 builds of size S or M with no builder
  set go to qwen for that run when the runner can build with qwen (it says so in its claim,
  `builders: ["qwen"]`); the lease records the builder, the item's own builder stays unset, so its
  next build is claude's again once the pace is normal. L items, a plan's `builder: claude` and a
  qwen item's fallback to claude are never routed.
- **Paused** at a budget, or while a limit hit's reset is ahead: claude takes nothing. Qwen builds
  (the item's or routed), land (a script) and regression runs (`scripts/e2e`, no agent) go on. A reset the CLI did not give, or gave in a form
  we cannot read or in the past, pauses for an hour: the next run then either works or hits the
  limit again at once, which costs nothing.
- **A frozen repo's main-red fix** (2026-10-18-main-red-freeze.md) still goes first and may take a
  slot over the active-agents number, but under the pace like any item: it is a P1, so it goes
  while slow; while paused claude does not build it (a fix with `builder: qwen` still goes). A run
  in a frozen repo is in the ledger when it finishes, though its advance waits for the thaw.
- **A held slot** (2026-10-18-slot-hold.md) is held only for an item the pace lets run:
  `SlotHolds` picks the held item under the same pace as the claim. Paused, no slot is kept for a
  Mac claude build nobody may start, so land and qwen builds on the other runners go on; slow, a
  hold stands only for a P1, size S or triage item while claude has room. A runner's builders are
  not recorded (only the claim at hand says them), so a build the pace would route to qwen holds
  no slot while claude is paced; the Mac runner still takes it, routed, when it claims a slot that
  is free anyway. The board's Runners panel shows the same holds.
- Every change of level (and of the number at once while slow) is a `usage_pace` event (no item),
  recorded at the claim that saw it or at the limit hit; the Slack bot posts it in `#loop-digest`:
  the hit with an @mention, the reset and "paused until"; the budget reached; pacing with the
  forecast; back to normal.

**This replaces "usage_limit sets the active agents to 0 and never raises them"** (DESIGN.md's old
"Automatic" paragraph). The number is the user's throttle again; a limit pauses claude claims until
its reset and they resume on their own. A run that hits the limit goes back to its queue, as before,
not to the user.

## What the user sees

- `GET /usage` (the tokens that read `GET /control`: user, runner, orchestrator), `loop usage`, and the board's **Usage** panel (open when
  paced): the 5h window's burn against its budget and its span, the forecast ("at this rate the 5h
  window runs out at …", from the window's average rate), the week, the pace, the last 7 days per
  provider (claude: burn, cost, runs; qwen: tokens, runs), the week's costliest items and the
  limits hit.

## Gaps, said plainly

- **Gemini/agy:** no Gemini runner exists yet (items #190/#191); the ledger's `provider` column and
  `BudgetProviders` are where it goes.
- **Plan and orchestrate turns** report no usage to loopd (their results have no metrics) and their
  limit hits do not pause claims; the next item run that hits the limit does.
- **The flow panel** (#201) is not built; its usage half is the board's Usage panel until then.
- **`rateLimitType` and `utilization`** are recorded but their exact values are as the Agent SDK's
  types name them, not checked against a live limit; the window falls back to the text.
- A runner newer than loopd sends `builders` in its claim when it has a qwen block, and an older
  loopd refuses the unknown field: loopd and the dev box's runner deploy together; a Mac runner
  with a qwen block must not be updated before loopd.
- `/loop usage 42%` stays advisory (the digest shows it); it is not used to learn the limit, since
  `/usage` shows both windows and the number does not say which.
