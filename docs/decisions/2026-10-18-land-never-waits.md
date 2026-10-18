# Land never waits for an agent slot, a resource or another item's CI (2026-10-18)

Item #213. **Seen:** at 11:00, 15 items sat in land for up to 2 h. Three causes, each its own fix:

1. loopd counted land leases against `active_agents` (`len(live) >= c.ActiveAgents`), though land
   runs no model; with 5–6 long builds a land claim rarely got a slot.
2. An item's resources applied in land, so the dev box (no `macos`) could not land a `macos` item,
   and #192's slot hold kept the last slot for `macos` items that were in land, which no Mac runner
   takes (its stages are build and review): the held slot served nobody.
3. Land was one item at a time (`limits.land` 1, and one runner land lane), and a land run keeps
   its lease while it waits up to `land_ci_wait` for CI, so the queue drained one CI run at a time.

This item supersedes #209 (PR #80) and #210 (PR #81), which were held unmerged; their commits are
on this branch unchanged, with their own decisions:

- [2026-10-18-land-needs-no-resources.md](2026-10-18-land-needs-no-resources.md) (#209, cause 2):
  `Item.Needs()` is an item's resources in triage, build and review and none in land; the claim, the
  lease's resources (the limits), the slot holds and the unservable flag all go by it.
- [2026-10-18-land-takes-no-agent-slot.md](2026-10-18-land-takes-no-agent-slot.md) (#210, cause 1
  and the hold): `AgentLeases` leaves land out of the active-agents count, a land item is claimed
  when the agent slots are full or held, and `SlotHolds` looks only at a runner's agent stages.

## Decision (cause 3)

- **Land's default limit is 3** (`pipeline.DefaultControl`). Migration `0022_land_limit.sql` moves a
  control row still at the old default of 1 to 3; any other value the user set stays.
- **A runner works land in `land_slots` lanes** (new runner config field, default 3), next to its
  agent `slots`; loopd's land limit still caps the total. A Reload resizes them as it does the agent
  slots. The sandbox's `Stack.Poll` counts land runs against `land_slots` the same way.
- **The lease is kept while waiting on CI.** With land taking no agent slot, a waiting land run
  holds only a land lane, and three of them now wait side by side. Releasing the lease while CI
  runs and re-claiming later (an item in land with a not-before time, the wait's start kept across
  claims) would change the land run and the store; it is not done here.

## Tests

- `TestLandProceedsPastSixBuilds` (pipeline): six builds in six agent slots and a build-and-review
  Mac runner around; the dev box's land claims take three land items, the `macos` one included, up
  to the limit, and nothing is held.
- `TestLandLanesWaitOnCITogether` (runner): three land items with CI pending are leased at once and
  all merge when CI passes.
- `TestLandLimitMigration` (store), and the land-limit cases in `TestChoose` / `TestLandTakesNoAgentSlot`.
- `scenarios/72-land-past-six-builds.json`: six hanging P1 builds take all six agent slots, then the
  dev box claims three approved items for land (one needing `macos`), they wait on CI side by side
  and squash-merge; a seventh build waits. With one land lane it fails at the second land claim.
- #209's and #210's scenarios, renumbered: `70-land-macos-item.json`, `71-land-past-full-agent-slots.json`.
