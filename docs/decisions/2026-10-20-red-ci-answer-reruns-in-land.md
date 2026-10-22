# The first red_ci answer returns the item to land, which re-runs the failed jobs once

2026-10-20 · item #341. **Seen:** #303 and #288 had one review bounce each, so their first red CI
in land was a bounce at the limit: `bounce()` asks "CI failed on `<sha>`: <jobs>" with return
stage **build**. The orchestrator's first `red_ci` answer ("land reads CI again: re-run it once")
went through the generic answer path to that return stage, so a build agent got the rerun text and
spent a whole run, and the user's Rebuild was refused (`wrong_stage: item is build`). Even when the
answer did go to land (the "build changed nothing" question), nothing re-ran the PR's CI: land read
the same red checks and sent the item straight back to build. What was chosen, each covered by a
test or a scenario:

- **Routing** (`pipeline.ApplyOrchAnswer`, called by `Store.OrchAnswer`). The orchestrator's first
  `red_ci` answer on a head to a question starting "CI failed on" returns the item to **land**,
  whatever stage asked, and the `answered` event carries `rerun_ci: true`. The second `red_ci`
  answer on the head goes to the question's return stage as before (build at the bounce limit).
  The user's answers are unchanged. A question that recommends a rebuild (main fixed, the item's
  `ci_main_fixed` set) gets no rerun: a rerun tests the same merge again, and that one is the
  user's.
- **The rerun** (`runner.rerunOwed`, `rerunPRFailed`). Land owes a rerun when the item's history
  has an `answered` event with `rerun_ci` on the PR's head and no `ci_rerun` event on that head
  since. When the gates then find red CI, land re-runs the failed jobs behind the failed checks
  (`gh run rerun --failed`, once per run), records `ci_rerun` (head, jobs, run ids; a new runner
  event kind, posted to the thread), waits one poll and reads the gates again: pending CI waits as
  usual (and goes back to the land queue as `ci_pending` past the wait), green merges, red again
  is today's `back_to_build`. The marker is the event, not a column: no migration, and a land run
  that stops after the rerun does not rerun again. GitHub refusing every rerun leaves the red
  standing (back to build). GitHub refuses to rerun a run still in progress, so while a run behind a failed check
  has a pending check, land waits (polling, within `land_ci_wait` and `land_ci_give_up`) before it
  reruns (#417, after #358 spent its rerun on a run in progress).
- **Wording:** the orchestrator skill (and its brief golden), scenario 59 (now expects the rerun)
  and a new scenario 85 (bounce, red CI at the limit, answer → land, rerun passes, merged, two
  build runs only).
