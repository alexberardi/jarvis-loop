# A reviewed rebase is pushed with a lease pinned to the PR's recorded head (2026-10-19)

Item #233. On #155 review asked for a rebase; the build rebased the branch in the runner's mirror,
but the new head changed a trusted file and its push waited on the user's Trust. After the Trust the
next claim failed: the mirror's branch and the remote's had diverged ("reconcile them by hand"), and
the user force-pushed by hand after checking with `git range-diff` that the new head was the old
one rebased. The user is fine with rebases on loop branches.

## Decision

The runner pushes a rewritten loop branch itself when the loop recorded both sides, and nothing
else:

- **Continuing the branch** (`Git.AddWorktree`, `Recorded`): when the mirror's branch and the
  remote's have diverged, the run starts from the mirror's when its head is one the user OK'd since
  (a Trust that still allows that commit, or an approval's head) **and** the remote is exactly the
  PR's last recorded head (`item.head_sha`). Otherwise the divergence is refused as before, and the
  item goes to the user: a remote commit the loop never recorded is never lost.
- **Pushing** (`publish`, `Git.Rewrites`, `Git.ForcePushBranch`): when the remote (fetched now) is
  exactly `item.head_sha` and the branch to push does not contain it, the runner pushes with
  `--force-with-lease=refs/heads/<branch>:<head_sha>`, never a bare `--force`, with the same
  `loop/`-only checks as the plain push. Anything that moved the remote in between fails the lease.
  This also covers a rebase done inside one run (no Trust in between). Every other push stays a
  plain fast-forward push.
- **Record**: a `branch_rewritten` event (a runner event kind loopd accepts) with `old`, `new` and
  `range_diff` (`git range-diff -s` over the base, clipped), so the item's history shows what was
  replaced. The PR is then updated and goes to review as any rebuilt PR.
- The push runs from the mirror with the runner process's own credentials, as `PushBranch` does
  (a Mac runner's too); no other credential path.

## Not decided here

The runner does not check that the rewrite keeps every old change (patch ids change with a
conflict resolution): review reads the updated PR, and the range-diff is in the item's history.
