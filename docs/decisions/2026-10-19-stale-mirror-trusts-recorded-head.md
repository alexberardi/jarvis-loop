# A stale mirror trusts the PR's recorded head (2026-10-19)

Item #283. Each runner keeps its own mirror. When runner B rebuilt and rebased an item's branch and
pushed it, runner A's mirror still had the old branch, and A's next prepare failed with "have
diverged; reconcile them by hand" (#255: the mini had the branch on old main, the remote had
b89b930, the head loopd recorded and review read). In #241 and #258 the runner had rebased the
branch itself and its push had failed. Each needed a push or ref fix by hand.

## Decision

`Git.AddWorktree`, when the mirror's branch and the remote's have diverged:

1. **A recorded rewrite** (#233, unchanged): the mirror's head is one the user OK'd and the remote
   is the PR's recorded head (`item.head_sha`): continue the mirror's.
2. **The remote is the recorded head**, and the mirror's is not an OK'd head:
   - if the mirror's commits are the remote's patches (`git range-diff -s` over the base, every
     commit `=`) on a strictly newer base (this runner's rebase whose push failed): continue the
     mirror's. `publish` pushes it with the lease pinned to the recorded head (`Rewrites`), as after
     a Trust, and records `branch_rewritten`. The build's own verify and Trust checks run on it as
     on any head.
   - otherwise the remote's is the truth (another runner pushed it; or a local rewrite with other
     patches): the mirror's head is kept under `refs/backup/<item>-<sha>` (`BackupRef`) and the
     branch is reset to the remote's. The runner logs the reset.
3. **Anything else** (no recorded head, or the remote is not it: someone else pushed): refused as
   before; the item goes to the user.

Nothing in the runner deletes `refs/backup/*` (the mirror's `fetch --prune` only prunes
`refs/remotes/`), so a reset never loses a commit.

## Not decided here

Pruning old backup refs; they are small and rare.
