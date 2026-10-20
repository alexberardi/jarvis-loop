# Verify's sandbox: its own name, a purge before each step, one retry on an environment failure (2026-10-20)

Item #327. On #265 (2026-10-20 09:37) jarvis-server's "pipeline scenarios on a sandbox" verify
failed in 1 s: `scripts/sandbox up --name loop-265` found the home that the build agent's harness
session and an earlier verify of the same item had left, re-ran seeding and failed with
`400 node_id already exists`. The same step had passed at 09:09. The item went to needs_you for
nothing.

The cause: the agent's harness and the runner's verify used the same sandbox name (`loop-<item>`),
and nothing purged it in between. The harness session's `down` runs only when the agent used
`loop-harness up`, and a verify step that fails or is killed before its own trailing
`down --purge` leaves its home for the next verify.

## Decision

- **Verify has its own name.** A verify step's `LOOP_SANDBOX` is `loop-<item>-verify`; the agent's
  harness keeps `loop-<item>`. A verify `run` should use `$LOOP_SANDBOX`, not a name it builds
  from `$LOOP_ITEM`.
- **Purge before each step.** When the repo has a harness, the runner runs its `down` (fenced, in
  the worktree, the step's environment, the harness timeout) for both names, one after the other,
  and waits for each before the step starts. This covers a config that still hardcodes
  `loop-$LOOP_ITEM`. A down that fails or times out is logged, not fatal: usually there is nothing
  to take down. The harness `down` must purge (`scripts/sandbox down --name $LOOP_SANDBOX --purge`).
- **One retry on an environment failure.** A step that exits 3 (the "the sandbox did not come up"
  convention that `PipelineCommand` uses: `up … || exit 3`) or fails the way `envFailure` means
  for proofs (exit 127, `[setup failed]`) is purged and run once more. Only a second failure is
  `verify_failed`. A step that passes on the second try says so in its status
  ("passed (…) on the second try (the first failed (exit 3, …))"), which the PR footer and the
  notes show. A repo with no harness gets no retry, since there is nothing to purge.

The retry depends on the exit code, not on matching the log for seeding errors. Matching
`node_id already exists` would tie the runner to jarvis-server's wording. The verify command
knows when its `up` failed, so `|| exit 3` lets it say so. With the purge in place, the collision
on #265 should not happen again even without the retry.

## For the runner config

The user's verify `run` for jarvis-server (and jarvis-node-mobile's from the server checkout)
should switch from `loop-$LOOP_ITEM` to `$LOOP_SANDBOX`, with `|| exit 3` after the `up`:
`scripts/sandbox up --name $LOOP_SANDBOX || exit 3; …; rc=$?; scripts/sandbox down --name $LOOP_SANDBOX --purge; exit $rc`.
