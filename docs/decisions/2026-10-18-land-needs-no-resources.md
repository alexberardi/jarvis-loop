# Land needs none of an item's resources (2026-10-18)

Item #209. #61 and #121 (resources `[macos]`) waited in land for about an hour, waiting on nothing:
the claim applied an item's resources to every stage, so the dev box (no `macos`) could not land
them, and the Mac runners do not take land (the runbook keeps landing on the dev box).

## Decision

An item's resources gate only the agent stages (triage, build, review). Land waits on CI, merges and
holds; it needs no Mac, GPU or emulator. `pipeline.Item.Needs` is an item's resources outside land
and nothing in land, and every rule that went by the resources goes by it:

- **The claim** (`pipeline.Choose`): capabilities ⊇ needs, `only_resources` (so a Mac runner gets no
  land item even without a stage list), the per-resource limits. Land's own stage limit (1),
  dependencies and the main-red freeze still apply.
- **The lease** (`store.Claim`) records the needs, so a landing `macos` item does not take the
  `macos` slot while CI runs.
- **Slot holds**: nothing is held for an item in land, and landing a `macos` item is not the held
  work.
- **Unservable**: an item in land is never flagged (`checkUnservable`), nor reported by the fake
  orchestrator's ready-but-unclaimed check.

## Not chosen

Letting the Mac runners take land: it would put the merge step and its credentials on a second
machine and tie landing a `macos` item to a Mac being awake, for no benefit.
