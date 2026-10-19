# Slow CI goes back to the land queue, not to the user (2026-10-19)

Item #278. **Seen:** on 2026-10-19 #253 (jarvis-server, `ci/upgrade (macos-14, darwin-arm64)`) and
#274 (jarvis-loop, `ci/check`) went to needs_you with "CI (…) still pending after 30m". Nothing had
failed: GitHub's macOS runners queued for 20 min and more that day. A land run that waited out
`land_ci_wait` reported `failed`, so a green-so-far item was parked with the user, the orchestrator
retried it with backoff as a run failure, and the waiting run held a land lane and its lease for
the whole 30 minutes. (Since #210 land takes no agent slot, so it did not hold one, as the ask
supposed.) #188 (jarvis-cmd-comfyui, "no checks reported yet") is the other case: no CI ever starts
on the head, and waiting longer does not help.

## Decision

- **A new land outcome, `ci_pending`** (`pipeline.CIPending`, allowed only from land in
  `approve.go`'s table). The item stays in land; it is not a failure (`IsRunFailure` does not count
  it, no bounce, no needs_you, no orchestrator retry). loopd records `not_before` = now + 10 min
  (`pipeline.LandRequeueAfter`), and the head and the time CI started pending on it
  (`ci_pending_sha`, `ci_pending_since`: the earliest a run reported for that head; a new head
  starts again). Every other outcome clears the three. Migration `0031_ci_pending.sql` adds them to
  `items`.
- **Claims skip a land item before its `not_before`** (`pipeline.Ready`, applied in `Store.Claim`).
  The flow panel names the wait "CI pending, checking again later".
- **The runner** (`ciWaitEnded`): when a run has waited `land_ci_wait` (unchanged, 30 min) and the
  PR has checks, it reports `ci_pending` with the wait, the head and the first wait's start. It
  reports `failed`, as before, when no check has reported at all (the #188 case), or when CI has
  been pending on that head for `land_ci_give_up` in all (new runner config, default `3h`; checked
  on every poll, so it fires on time rather than at the end of a run). GitHub's mergeability wait
  is treated like pending CI, under the same cap.
- **Quiet in Slack:** neither the `ci_pending` advance nor the next run's claim posts in the
  thread; the parent's stage line says "waiting for CI (…) since 14:05 UTC, checking again at
  14:45 UTC". `land_waiting` fires once per head, not once per run.
- `land_ci_wait` stays 30 min: a run that holds its lane that long is no longer costly with three
  lanes and requeueing, and a shorter wait would mostly add claims.

## Tests

- `TestCIPendingStaysInLand`, `TestCIPendingRefusals`, `TestClaimSkipsDeferredLand` (pipeline);
  `TestCIPendingRequeuesLand` (store: round trip, no claim before `not_before`, claim after);
  `TestCIPendingAtLimit`, `TestLandCIGiveUpConfig` (runner).
- Scenarios `77-ci-pending-requeue` (pending past 30 min → back in land, unleased, nothing posted;
  claimed again after 10 min; CI passes → merged) and `78-ci-pending-give-up` (four `ci_pending`
  runs, then needs_you "CI (check) still pending after 3h"). `19-ci-timeout` (no checks → needs_you
  after 30 min) is unchanged.
