# Leases belong to the runner token; evidence counts per lease

2026-10-14 · phase 2 (loopd core)

**Decisions.**

- **The runner is the token.** A lease is held by the runner named in the tokens file. `POST /claim`
  may send `runner`, but it must equal the token's name (403 `runner_mismatch`); it defaults to it.
  A token cannot claim, heartbeat, advance or upload as another runner.
- **No lease id on advance or evidence.** The spec's CLI is `loop advance <id> <outcome>`, with no
  lease, so the server finds the item's one live lease (a unique partial index guarantees at most
  one) and checks that the calling runner holds it: no live lease → 409 `no_lease`, someone else's →
  403 `not_your_lease`. Heartbeats do name the lease (`/leases/{id}/heartbeat`).
- **"≥1 evidence" means under this lease.** Each evidence row records the lease it was uploaded
  under, and an outcome that needs evidence (`reproduced`, `not_reproducible`, `pr_opened`) counts
  only files attached during the stage being finished. Evidence from an earlier stage does not
  satisfy a later one.
- **Stop ends the lease at the next heartbeat.** With `stopped` set, a heartbeat answers
  `{stop: true}`, ends the lease (event `stopped`) and the item waits in its stage again, as
  DESIGN.md's "each item returns to its queue". A heartbeat on a lease that already ended (expired,
  overtime, stopped) also answers `stop: true` rather than an error, so a runner has one rule.
- **Overtime beats expiry.** If a lease is both past the max run time and unrenewed, it goes to
  needs_you ("run exceeded N minutes"), because the user should hear about a run that long.
  Only when it was still alive at the cap, though (2026-10-24, #592): a lease that expired
  before the cap stopped because its runner went away (a host crash, a shutdown), so it is
  expired however late loopd sweeps it, and the item waits in its stage again. Its
  `lease_expired` event carries the run's last reported tool call (`last_tool`).

**Why.** Each is the simplest reading of the spec that keeps one runner from acting for another,
and keeps the evidence gate meaningful.
