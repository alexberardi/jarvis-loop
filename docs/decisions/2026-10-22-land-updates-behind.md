# Land updates a BEHIND branch instead of merging as-is

2026-10-22 · item #521. **Seen:** #496 (jarvis-node-mobile PR #130) failed land twice with
`gh pr merge: … not mergeable: the head branch is not up to date with the base branch` and went to
needs_you; it moved only after a manual `gh pr update-branch 130` and a green re-run. The repo now
has GitHub's "require branches to be up to date before merging", which the land spec assumed no
repo had ("behind but clean merges as is"). What was chosen, each covered by a test or a scenario:

- **Gate 3d, keyed on `mergeStateStatus == BEHIND`** (`runner.Decide` → `ActUpdate`). GitHub
  answers `BEHIND` only when the base requires up-to-date heads, so the gate needs no per-repo
  config and holds for every repo. It sits after the conflict (3b) and part-of-the-ask (3c) gates —
  a conflicting or partial PR never merges, so updating it first is waste — and before CI (gate 4),
  because waiting on the checks of a head that cannot merge is the wait #496 paid twice.
- **The update is `mergeBase`, not `gh pr update-branch`:** merge `origin/<base>` into the branch
  in a scratch worktree and push, exactly as the conflict path (`landConflict`) and the held watch
  (`heldMerge`) already do. It reuses their push rules (only `loop/<item>-…`, never forced), keeps
  the merge commit the runner's own, and works against the sandbox's fake GitHub, which has no
  update-branch. A conflict outside docs/ goes `back_to_build` with the files (a real conflict
  escalates to the builder, not the user); on success a `land_update` event records `old_head`,
  `new_head` and `how`, and the run `continue`s so every gate re-derives on the new head (land's
  no-state invariant). At most `MaxLandUpdates` (3) updates per run: past the cap the run is
  `failed` ("the base keeps moving under it") — a bound, like the two merge refusals.
- **The review approval carries over clean merges of the base,** or every update would bounce to
  review and re-arrive behind again. In `landFacts`, when the reviewed head is not the PR's head,
  the runner computes `ApprovalDelta` (pure git facts, as the held-approval carry of
  `docs/decisions/2026-10-18-approval-carry.md`); the review approval moves to the new head only
  when the delta is merges of `origin/<base>` alone with **no conflicts at all** — stricter than
  `Carryable`, since nobody reviewed a conflict resolution. The content is then git's own merge of
  the reviewed head and the base, which is what GitHub would have tested anyway. The `merged` event
  names `review_carried_from`. A union-docs merge (conflicts, even only in docs/) still goes back
  to review, as before; so does a rebase, a foreign commit or a merge of anything else. This also
  fixes the same loop when anyone else merges the base into the branch by hand.
- **A held item that is BEHIND is updated too,** and its hold approval does not carry: gate 6 sees
  the approval naming the old head and reports `hold` for the new one, so the user approves again
  (loopd's `checkMerge` requires the approval to name the merged head). Chosen over skipping the
  update for approved held items, which would leave them in the refusal loop this decision removes;
  the extra approval is the hold list working as designed.
- **Fake GitHub models the protection** (`require_up_to_date` on a `github` scenario step): a clean
  head that lacks the base's tip is `MERGEABLE`+`BEHIND`, and `Merge` refuses it with gh's wording
  from #496. Scenario `96-land-updates-behind-branch` proves the flow end to end: main moves under
  an approved PR, land updates, CI re-runs on the new head, the merge lands with
  `review_carried_from`, and nothing reaches needs_you or a second review.
