# A held approval survives a merge of the base (2026-10-18, #217)

**Problem.** An approval is for one commit (`Approval.HeadSHA`; `checkMerge` lets only that head
merge). A held PR the user approved often conflicts with main by the time land runs: land (docs
only) or a build (code) merges `origin/main` in, the head moves, the approval was dropped, and the
item came back to the user (#194 cycled held → conflict → rebuild → review → held several times).

**Decision.**

- **A new head no longer drops the approval; it voids it.** `voidApproval` still records
  `approval_voided`, but the approval stays on the item, for its old head. Every gate already
  compares heads (`checkMerge`, land's `ApprovedHead`, the runway), so a void approval merges
  nothing. It is dropped where the user's word or a new held message decides instead: land's
  `hold` with a new head (the held message asks for that head), the user's Keep holding, the
  user's rebuild (they asked for a change), main-red rebuilds (unchanged). The runner's own
  conflict rebuild of a held PR keeps it (`ApplyRebuild(…, conflict)`).
- **Review computes the delta** (`runner/carry.go`, `Git.ApprovalDelta`) when the item's approval
  is for another head than the one reviewed: the approved commit must be in the new head's
  history; every commit since must be a two-parent merge whose second parent is on `origin/<base>`;
  each merge is redone with `git merge-tree --write-tree`, and the merge commit must equal git's
  own merge in every file except the conflicted ones, where every line outside the conflict
  markers must be kept, in order (`keptOutsideConflicts`). A file that breaks either rule is
  "changed" (the PR's own lines). The delta, with each merge's `git show --remerge-diff`, is the
  reviewer's `.loop/context/approval-delta.md` and an evidence file `approval-delta.md` the runner
  writes after the agent (an agent's file of that name is replaced). The approved review sends it
  as `carry_*` fields.
- **loopd decides** (`pipeline.carryApproval`, in `approve.go`, on the hold list): on review's
  `approved`, an approval for an older head moves to the new head (`CarriedFrom` = the old head)
  only when the delta is from that approval's commit, has merges and nothing else, no changed file,
  no conflicted file on the hold rules (the rules see each conflicted path as `M path`), and the
  reviewer did not mark it needs review. The event `approval_carried` has both shas, the approver,
  the merges and the conflicted files; the item's thread gets a line. Otherwise
  `approval_not_carried` (with the reason) drops it, and land holds the new head for the user.
- **A held PR is kept mergeable.** The held watch (`CheckHeld`) used to send every CONFLICTING
  held PR to build. It now tries land's merge first (`mergeBase`, shared with `landConflict`): a
  clean merge or one whose conflicts are all under `docs/` is pushed and reported with the new
  route `POST /items/{id}/held-merge` (runners only; `pipeline.ApplyHeldMerge`, held → review,
  event `held_merge`). Like the runner's rebuild it holds no lease: it moves only a held item, and
  only to review, which must approve the new head before land runs; the PR's branch must be the
  item's (`loop/<id>-…`) and is pushed only forward. This replaces the "first cut" of
  [held-back-to-build](2026-10-16-held-back-to-build.md); a code conflict still goes to build.

**Not done.** "Identical outside conflict hunks" is a textual rule: a clean merge that changes what
the PR's code means (main renamed a function the PR calls) carries; the review of the new head,
which has the delta and the remerge diff, is what catches that. A rebase is never carried (the
approved commit is not in the history): builds are told to merge.
