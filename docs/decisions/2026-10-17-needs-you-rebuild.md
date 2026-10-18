# Rebuild from a needs-you thread, a Rebuild button, and "main fixed" red CI

2026-10-17 · item #131. **Seen:** #123 and #124 hit red CI on a head the build did not change
(needs-you, return stage land). The user replied "rebuild, rebase on main", which was taken as an
answer: the item went back to land, which read CI again on the same stale merge ref and failed
again. Only a held thread knew a builder's note (`IsHeldNote`), and `pipeline.ApplyRebuild`
refused anything not held, so the user had to call the API by hand. What was chosen, each covered
by a test or a scenario:

- **Needs-you → build** (`pipeline.ApplyRebuild`, event `rebuild`, data `notes` and `question`).
  It now takes held or needs_you items. From needs_you it clears the question and return stage;
  the PR, branch and `ci_red_sha` stay; it is not a bounce. Only a question asked **after
  triage** (`pipeline.Rebuildable`: return stage build, review or land) can be rebuilt; triage's
  questions have no repo or build yet, so a "rebuild" reply there stays the answer. A runner's
  rebuild (the held watch's conflict) still moves only a held item (`store.Rebuild` refuses a
  `runner:` actor on anything else), so it can never answer a question that came up meanwhile.
- **The reply (Slack).** In a needs-you thread, the approver's reply is a rebuild note when
  `IsRebuildNote` says so, otherwise it is the answer, as before. This rule is narrower than
  `IsHeldNote` on purpose. An answer to a question often opens with a change verb ("use the
  staging DB", "add it to jarvis-node"), and "retry" and "drop it" mean what they say there. So
  after mentions and lead words the reply must open with **rebuild, rebase or fix** (or their -ing
  forms), and "fix main …" stays an answer (the red-CI question offers "fix main"). Questions and
  negations are never notes. Both directions are in `TestIsRebuildNote`. Someone else's reply is
  refused as before.
- **The Rebuild… button** sits on the needs-you message (only when the question came after
  triage) and on the held message, next to Drop. It is the approver's only, like Approve: someone
  else's press is refused, recorded and opens nothing. The approver's press opens a modal
  (`views.open` with the press's `trigger_id`) that has one optional note. The note starts as
  "rebase on main and fix what CI shows" (`slack.RebuildDefaultNote`), which is also used when the
  box is left empty. On `view_submission` the bot checks the approver again, cancels any pending
  reply approval and calls `Store.Rebuild`. If the item moved on meanwhile, the store refuses and
  its event line says why in the thread. The needs-you message now says: Reply in this thread to
  answer, or start with "rebuild" (or use Rebuild…) to send it back to build with your note.
- **The builder sees it.** The `rebuild` notes since the last `pr_opened` go to
  `.loop/review/held.md` on every build, with or without a PR (before, only when the item had a
  PR). A needs-you rebuild's entry names the question it replaced. The brief line is "Sent back to
  build by the user instead of an answer to its question; PR #n is open on this branch. Do what
  `.loop/review/held.md` asks …" (without the PR part when there is no PR yet).
- **Red CI whose failure was main's.** At a red CI, land compares main's CI at the PR's merge base
  with main's CI at main's head (`Git.MergeBase`, `CommitRuns`, `runner.MainFixed`). If the base
  was red and the head is green (a different commit), it sends `main_fixed_sha` (main's head) on
  `back_to_build` and appends "main was red at this PR's base and is green now at `…`: rebase on
  main" to the reason, so the builder sees it in the bounce notes too. loopd keeps it as
  `ci_main_fixed` (migration 0018), and if the build then changes nothing the question says: CI
  failed on X and the build changed nothing, but main was red at this PR's base and is green now
  (`Y`): rebuild (rebase on main), not a rerun, which tests the same merge again. Reply "rebuild"
  or use the Rebuild button, or drop. The orchestrator's standing `red_ci` answer (re-run once)
  does not apply to that question (skill §5 and the persona): it is the user's.

**Not done / first cut:** "the failing tests failed on main then" is approximated by main's whole
CI result at the merge base. The PR's failing check names are not matched against main's failing
runs, because a check is a job and a main run is a workflow, so their names differ. Main's history
between the base and the head is not walked either. The orchestrator gets no rebuild action of its
own: a rebuild is the user's. There is no `loop rebuild` CLI command (the API route takes a user
token). The needs-you and held messages' buttons are not updated when the item moves on, as before.
