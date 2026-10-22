# One live run per item (#453)

## Problem

On 2026-10-21 (rev 5d7397b) #429 and #430 each had two agents start back to back on the same
worktree (`work/<id>-build`). The first was then told to stop at its heartbeat (exit 143), and its
cleanup removed the worktree under the second, which found no `.git` and returned a question.

loopd already allows one live lease per item (`leases_one_live`). The gap was on the runner:

- `prepare` (the mirror lock, which up to 10 slots and land, verify and proofs share; the fetch;
  the worktree; the repo's Prepare commands, up to 20 minutes each) sent **no heartbeat**, so a
  slow setup outlived the 5-minute lease TTL. loopd ended the lease `expired`, and the item could
  be claimed again, by another slot of the same runner, while the first run went on.
- Both runs use `WorkRoot/<id>-<stage>`, and `cleanup` removed it unconditionally.
- The same expiry explains the `advance … 409 no_lease` after a `start_failed`: the lease had
  ended during prepare.

## Decision

1. **The lease stays alive while the run is set up.** `prepareAlive` runs `prepare` while
   heartbeating (`keepAliveIn`, phase `setup`, note "preparing the run"), up to the run's time
   cap. When loopd says stop or the lease is gone (4xx), prepare is cancelled, no agent starts and
   nothing is advanced: the run ends as a stop does mid-run. A prepare past the time cap fails the
   run.
2. **One run per item on a runner.** `Runner.live` maps an item to its run until that run's
   cleanup is done. A claim made while an earlier run of the item is still ending **waits** for
   that cleanup, heartbeating its own lease, before it touches the worktree. It does not give the
   claim back, because loopd has no release call and a run that had just stopped would be claimed
   again at once. The earlier run notices within a heartbeat (its lease ended) and cleans up.
   If the wait outlasts the run's time cap, the item fails with the reason.
3. **A cleanup removes only its own worktree.** `prepare` writes the run's lease into
   `.loop/lease` (git ignores `.loop/`). `cleanup` runs no cleanup command and removes nothing
   when that file names another lease, or when another lease holds the item on this runner. It
   logs `worktree taken over by a later run; not removing it`.

Items' agent runs (triage, build, review) only. Plan and orchestrate turns use their own
directories (`plan-<id>`, `orchestrator`) and leases, and land, verify and proofs heartbeat
already.

## Tests

`internal/runner/liverun_test.go`: the lease is extended through a 12-minute prepare, and a stop
there starts no agent; a second claim waits for the first run's cleanup, and one agent runs; a
taken-over worktree survives cleanup. Scenario `91-slow-prepare.json`: a 7-minute Prepare keeps
the lease, and a second slot claims nothing.

## Not done

Two runner **processes** on one work root (not a supported setup) are covered only by the
`.loop/lease` marker, not by the in-memory registry.
