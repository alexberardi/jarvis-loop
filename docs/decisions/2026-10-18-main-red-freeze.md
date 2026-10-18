# Main red: a P1 at once, and the repo freezes until green

2026-10-18 · item #197. **Seen (2026-10-17):** a test broken on main
(`TestIdleQueueLeavesTheWriterAlone`) bounced two good PRs (#123, #124), and a rerun could not
help. The user's rule: a red main is an emergency. What was chosen, each covered by a test or a
scenario:

- **Red means red after one re-run.** The runner's main watch (`watchMain`) re-runs the failed
  jobs of a red result once (`gh run rerun <id> --failed`, `LandGitHub.RerunFailed`) and keeps
  polling, with a fresh `land_main_wait`. Green then is a flake: `main_ci` green with `rerun`
  naming the jobs, no P1, no freeze (scenario `main-red-flake`). Red again is red.
- **loopd files the P1 and freezes, in one transaction** (`store.MainCI` → `mainRed`). The runner
  no longer files the P1 itself: it reports `main_ci` red with the failing jobs, the runs, the
  merge and `last_green` (main's head the merge was checked against, when its CI was green), and
  loopd files "main red in `<repo>`: `<jobs>`" (P1, source finding) with an ask naming the first
  red commit, the last green one, the likely culprit (the merge between them) and that a revert of
  it is fine when the fix is not obvious (`pipeline.MainRedAsk`). While a freeze's P1 is open, a
  second red reuses it (`reused`); a red after the P1 is done (its fix did not fix it) files a new
  P1 for the same freeze (`again`). The P1's triage still gets the failing log (`MainRedCommit`
  reads the ask; it knows both titles). A runner older than this change still files its own P1 and
  sends `p1_item`, which loopd drops, so mixed versions file two; deploy runner and loopd together.
- **The freeze** (`repo_freezes`, migration 0019; events `frozen`/`unfrozen`, no item) holds the
  repo's P1 and `red_since`, when the culprit merged. In a frozen repo:
  - **No claims** (`pipeline.Frozen`, `Choose`/`ChooseWork`): its items are skipped; items with no
    repo yet (triage) and other repos keep going.
  - **The P1 goes first**, through every stage (`chooseFix`): before any other item and before the
    orchestrate turn, and over the active-agents number and its stage's limit ("it may take a held
    slot": the frozen repo's own runs keep theirs). A stop, 0 active agents, resource limits and
    review independence still apply. A runner's own `slots` still bound it: with one slot busy on
    a frozen run, the P1 waits for that runner's next free slot (or another runner).
  - **Runs already going finish; their advance waits** (`queued_advances`). `Store.Advance` checks
    the outcome now (a bad report is refused at once, as before), records it with its fields,
    paths and lease, ends the lease (`queued`) and leaves the item where it is
    (`advance_queued`); no claim takes a queued item. A usage limit is about the runner's account,
    not the repo: it applies at once. The runner's own side effects (a pushed branch, an opened or
    updated PR) happen as usual; only loopd's stage move waits.
  - **No merges.** A land run in a frozen repo stops at its next pass of the gates, before any
    merge: `Detail.frozen` tells the runner, and its heartbeat makes loopd end the lease
    (`land_frozen`); the item waits in land and is landed afresh after the freeze (scenario
    `main-red-land-stops`). Land's other outcomes from a pass already decided are queued like any
    advance.
  - **Human actions still work** (answer, approve, drop, rebuild): they need no runner and do not
    merge; an approved item just waits in land. A drop of an item whose advance waits drops the
    advance (`queued_advance_dropped`).
- **Lifting.** Automatically when `main_ci` is green on a merge after the red one (normally the
  P1's own; a green from a merge before the culprit says nothing about main now), or by hand:
  `loop unfreeze <repo>` (`POST /unfreeze`, the user's token only; `GET /freezes` for everyone).
  The queued advances then apply in the order the runs finished, through the same
  `pipeline.ApplyAdvance` (re-checked against the item as it is now; one whose item moved, or no
  longer passes, is dropped with the reason). Then every item of the repo that land sent back for
  red CI in the red window (since `red_since`) and that has not moved since goes back to build with
  "main was red; rebase and re-run" (`pipeline.ApplyMainRedRebuild`, event `rebuild` by loopd,
  `main_red`), from build or from a rebuildable needs-you; its brief says to merge main in (the
  runner never force-pushes, so no rebase) (scenario `main-red-unfreeze`). Queued advances are not
  stale-checked beyond that: a review approval binds its head as always, and land re-derives
  everything from GitHub on its next claim.
- **Announced.** #loop gets a post on freeze (the jobs, what is frozen, the P1, the culprit, how
  to lift it) and on unfreeze (what applied, what was dropped, what was rebuilt); an item thread
  gets a line when its result waits, when its land run stops, and when a waiting result is
  dropped. The board shows a red banner per frozen repo; `loop status` a FROZEN line.

**Not done:** red from a direct push to main (not a loop merge) is still not seen: only merges the
loop watched freeze a repo. A periodic check of each repo's main head is the follow-up; until then
the user can see it on GitHub, and lift by hand. A freeze lifts only on a green watched merge or by
hand: a fix pushed outside the loop needs `loop unfreeze`.
