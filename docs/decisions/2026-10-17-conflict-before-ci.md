# Land: a conflicting PR takes the conflict path before CI (2026-10-17)

Item #91. GitHub runs no `pull_request` CI on a PR that conflicts with its base. Land checked CI
(gate 4) before mergeability (gate 7), so a PR that turned conflicting before its checks ran waited
for checks that never came and, after `land_ci_wait` (30 min), asked the user "CI (no checks
reported yet) still pending after 30m" (#72 / PR #48, after main moved).

## Decided

- **`runner.Decide` checks for a conflict right after the changed paths** (gate 3), before CI:
  `mergeable == CONFLICTING` or `mergeStateStatus == DIRTY` (also with `mergeable` `UNKNOWN`) →
  `ActConflict`, the existing path in `land_conflict.go` (docs/-only conflicts: union merge, push,
  `back_to_review`; anything else: `back_to_build` naming the files).
- **A conflict beats red CI, missing evidence and hold.** The conflict path never merges: either
  the branch gets a new head (which goes through review and every gate again) or the item goes
  back to build, where the builder sees the conflict. A red CI on the old head is moot once the
  head must change, and a held PR that conflicts already goes back to build from the held watch
  (`2026-10-16-held-back-to-build.md`). The old table row "hold beats a conflict" now expects the
  conflict.
- Gate 7 keeps `MERGEABLE` → merge and anything else (`UNKNOWN`, empty) → wait.

## Verified

`TestDecide` rows for: conflicting with no checks, with CI pending, `DIRTY` with mergeability
unknown, conflict vs red CI / missing evidence / hold, and the earlier gates still winning over a
conflict. Scenario `59-conflict-no-ci`: main moves before any CI ran; land sends the item back to
build at once, with no `land_waiting` event.
