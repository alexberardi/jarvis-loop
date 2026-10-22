# A reopen closed not reproducible sends the done item back to unproven (#415)

## Problem

A done item goes to `verify=failed` when its proof's command fails twice or the user reopens it in
qa, and the loop files a reopen item (`reopen_of`). When triage then closes that reopen
`not_reproducible`, only the reopen changed: the original stayed failed for good, and the morning
report counted it. On 2026-10-21, 27 of the 29 done items marked failed were like that, nearly all
proof-environment problems (no pytest, docker not found, missing node_modules, a mise error).
PR #101 (#282: a proof's setup or environment error never reopens) stopped new environment failures from
reopening, but not the items already failed, nor a future reopen that is not reproducible.

## Decision

- When an advance closes a reopen `not_reproducible` (`store.applyAdvance`), and it is the newest
  reopen of its original, and the original is a failed done item, the original goes back to
  `unproven`: its proof is dropped, `verify_asked` is reset so the done sweep asks the orchestrator
  for a new proof (the "re-run its proof" part), and its reason says `reopen #N closed not
  reproducible: <summary's first line>`. The event is the original's, kind `done_reproof`
  (`pipeline.ApplyReopenNotReproducible`), and its Slack thread hears it.
- **Any** reopen counts, the user's from qa too: the item is merged and done either way, and
  triage found nothing wrong with it.
- **Except a duplicate:** a summary starting with "duplicate" means the failure is real and tracked
  by another item, so the original stays failed.
- A newer reopen of the same item still open keeps it failed: that one has not been decided.
- `0038_reopen_not_reproducible.sql` does the same for the items failed before this change (no
  events: migrations write none, as `0033` did not).

## Not done

- A reopen that is dropped (`ApplyDrop`) rather than closed not reproducible leaves the original
  failed.
- A failed done item whose reopen was merged as a fix stays failed; whether a fix should verify the
  original is a separate question.
