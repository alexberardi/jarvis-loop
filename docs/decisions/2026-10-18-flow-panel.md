# The flow panel: how the loop is doing, from the events (2026-10-18)

Item #201. "How are we doing?" was answered by hand with SQL on loop.db. The board now has a flow
panel (`/board/flow`, linked from the board's header) and the API the same numbers as JSON
(`GET /flow`, any reading token: user, runner, orchestrator), for a window of 6h, 24h (default) or
7d ending now. Both read `store.Flow`; nothing new is recorded except a runner's slots.

## What each number is

- **Done** counts the events that moved an item into `done`; **merged** the events with outcome
  `merged` (into done or qa). A merge into done counts for both. Per hour: one bucket per hour from
  the window's start, drawn as two bar sparklines (one series each, the hour and count in each
  bar's tooltip) and also given as the totals and per-hour rates.
- **Time per stage** is per **pass**: from the event that moved the item into a stage to the next
  event that moved it out (`from_stage != to_stage`), counted when the pass ended in the window
  (it may have started before). A bounce makes a second build and review pass, and each is a
  sample; the bounces are counted separately. Notes that keep the stage (`from = to`) do not end a
  pass. Stages: triage, build, review, land, qa, needs_you, held. **Run time** is the agent runs of
  the stage that ended with a result (`leases.end_reason = advanced`), claim to end, so a stage's
  pass time minus its run time is the queue.
- **Verify** has no stage of its own: the runner's verify runs inside the build run, so it is part
  of build's pass and run time. Its failures are counted as the `verify` row of the bounces.
- **End to end** is from the item's `created_at` to its merge (`to_merge`) and to done (`to_done`).
- **Median and p90** are nearest-rank (`pipeline.DistOf`: the value at rank ⌈p·n⌉), computed in
  Go: SQLite has no percentile function, the samples need the per-item pass pairing anyway, and a
  7d window is a few thousand samples.
- **Why items wait now** classifies every open item that waits (`pipeline.WaitReason`): `needs
  you`, `held for approval`, `qa`; for an unclaimed item in a work stage: `dependencies`, `no
  runner for a resource` (the `unservable` flag), `stopped`, `no free slot` (agent runs ≥ active
  agents, turns included; land leases take none), `limit <key>:<n>` (a resource's limit first, then the stage's), else
  `ready` (nothing holds it: the next runner that asks for its stage gets it). The item's own
  blockers come before the loop's (once the loop frees up it would still wait for them).
  `WaitReason` and the claim (`Choose`) share the checks (`loopBlocks`, `itemBlocks`), and
  `store.Flow` uses the claim's own state (`claimState`: live runs, frozen repos, held slots), so the panel cannot say "ready"
  for an item the claim would skip for a limit or a dependency. Review independence (the builder's
  agent may not review) depends on who asks, so it is not a reason.
- **Sent back** is the `advanced` events with outcome `bounce` (counted against review),
  `verify_failed` (verify), `back_to_build` or `back_to_review` (land), with each outcome's count and
  the top 3 reasons: the first non-empty line of the notes or reason, cut to 100 characters,
  grouped by that text, most often first, with the items. Free-text review notes rarely repeat, so
  that list is mostly the latest reasons; red CI and verify failures do repeat.
- **Slot use**: busy agent-minutes per runner are its runs' overlap with the window (item leases,
  plan-turn leases, orchestrate turns; a live run counts to now). A runner now sends its `slots`
  with each claim (`runners.slots`; 0 for a runner that predates it, shown as `?`), so capacity is
  slots × the window for a runner seen in it, and idle what of it was not busy. That overstates
  idle for a runner that was off part of the window (loopd does not record when a runner came up);
  the loop-wide share (busy ÷ active agents × window) is on the tile.
- **Killed runs** are the leases (and plan-turn leases) that ended `expired`, `overtime`,
  `stopped`, `dropped` or `frozen` (a land run stopped before its merge because its repo froze) in
  the window; their whole run time is the lost agent-minutes. A run that ended `queued` (its
  advance waits for the unfreeze) kept its work and is not killed.
- **Features** are the plans with an open item, or an item that moved in the window: items by
  stage and done/total. Items carry no feature tags on main yet (#198); when they do, the panel
  groups by tag as well.
- **Frozen repo** (#197, 2026-10-18-main-red-freeze.md): an item of a repo whose main is red waits
  `frozen repo`, after its own blockers and before the loop's. The repo's fix is exempt, as the
  claim's `chooseFix` takes it: it waits only for a stop, 0 active agents or a resource limit, not
  for a free slot or its stage's limit (`itemBlocks` takes the same `stageLimits` switch as the
  claim's `choose`).
- **Held slots** (#192, 2026-10-18-slot-hold.md): when the free slots are all held for a Mac's
  work (`SlotHolds`), an item that needs none of the held resources waits `no free slot`, after
  its limits (`ChooseWork` applies the holds after `Choose`); the held work itself is `ready`, and
  a frozen repo's fix ignores the holds as `chooseFix` does. The runner panel on the board names
  the hold; the flow panel keeps the ask's reasons.
- **Land** (#209, #210): land runs no agent, so a land item waits on none of its resources' limits
  (`itemBlocks` uses `Needs`) and never for a busy or held agent slot; only its stage's limit, a
  stop or 0 active agents hold it back.

## Speed

Migration 0024 adds `leases(ended_at)` and `plan_leases(ended_at)`; `events(at)` comes from the
runway migration (#205, `0023_runway.sql`), so this one does not create it again. Every window query
starts from one of the three (`TestFlowQueriesUseIndexes` checks the query plans), and the per-item
pass pairing reads only the items with a stage change in the window, by the existing
`events(item_id, id)` index. `BenchmarkFlow7d` (100k events over 2k items, 20k leases) takes about
30 ms for the 7d window on the dev box. The live loop.db was not measured (off limits to the build).

## Rollout

The claim's new `slots` field is refused by an older loopd (the API rejects unknown fields), so
loopd must be deployed no later than the runners that send it, as with `only_resources`. The dev
box's loopd and runner are built from one commit and deployed together
(2026-10-16-qa-after-deploy.md); a Mac runner updated ahead of loopd would have its claims refused
until loopd catches up.
