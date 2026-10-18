# Land takes no agent slot (2026-10-18)

Item #210. **Seen:** at 13:58, 12 items sat in land and none was being worked, while the dev box
claimed only build, review and triage (and on 2026-10-17 at 16:06, 19 items). The runner's land lane
(#24, [2026-10-16-land-lane.md](2026-10-16-land-lane.md)) was meant to keep an approved item from
waiting for a slot, but loopd still counted land leases against `active_agents` and refused the land
claim while the agent slots were full. An agent lane that finishes claims again at once, and the land
lane asks only every `poll_seconds`, so a freed slot always went to agent work first. #192's held
slot refused land too ("slot held for macos work").

## Decision

Land runs no agent, so in loopd's claim rules (`pipeline.ChooseWork`, `Choose`, `SlotHolds`):

- **Land leases are left out of the active-agents count** (`AgentLeases`). A live land lease takes
  no agent slot, and it does not make the last free slot for a hold.
- **A land item is claimed when the agent slots are full** (`chooseLand`): with every slot busy, a
  claim gets only a land item, or nothing.
- **Held slots don't refuse land.** A claim whose pick is a land item gets it, and a claim refused for
  a held slot still gets a land item if there is one.
- **A land item holds no slot.** `SlotHolds` looks only at a runner's agent stages (a runner with
  only `land` holds nothing), since landing a Mac item needs no agent slot.

What still applies to land: `/loop stop`, `active_agents` 0 (the usage-limit stop and the user's
"no agents"), the land limit (1), the resource limits, a frozen repo, and the dependencies.

Tests: `TestLandTakesNoAgentSlot` (pipeline), `TestClaimLandsWhenAgentSlotsAreFull` (store), and
`scenarios/71-land-past-full-agent-slots.json`: `active_agents` 1, a hanging P1 build in the one
slot, and an approved item still lands next to it.

## Not covered

Land is still one item at a time (`limits.land` 1), and one land run can wait up to `land_ci_wait`
for CI, so a long queue drains slowly. That is throughput, not the stall.
