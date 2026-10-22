# A claim matches the runner's repos too (2026-10-22)

Item #467. #455 (jarvis-node-mobile, resources `macos`) was claimed by mbp twice and by mini once;
each run failed at once with `runner <name> has no repo "alexberardi/jarvis-node-mobile"
configured`. The claim matched on capabilities only, so loopd handed the item to runners that had
no checkout of its repo, and nothing flagged it as unservable.

## Decision

- **The runner sends its repos with every claim** (`repos` in `POST /claim`, both lanes): every
  name its config resolves an item's repo to (`Config.RepoNames`: the config keys, the slugs, and
  the lowercased aliases and slug names).
- **The claim rule** (`pipeline.choose`): resources ⊆ capabilities **and** the item's repo is one
  of the runner's (`pipeline.HasRepo`: the name as it is or lowercased, as the runner's config
  matches it). An item with no repo (triage picks it) goes to any runner; a runner that sends no
  repos predates them and takes any repo, so an old runner claims as before. Land goes by it too.
- **loopd remembers them** (`runners.repos`, migration 0040), so the slot holds
  (`SlotHolds`) hold a slot only for a runner that has the repo, and the orchestrator snapshot
  shows each runner's repos.
- **Unservable** (docs/decisions/2026-10-17-unservable-items.md): a runner seen lately serves an
  item only with its resources and its repo. The item's repo is named among the missing (`no runner
  offers sandbox/extra`, or `macos + o/x` when one runner has macos and another the repo), in every
  work stage including land (which needs no resources but runs in a checkout). An item with no
  resources is judged on its repo only while some runner has been seen. The item keeps waiting in its
  stage and is claimed once a runner with the repo (a reloaded runner.json) asks.

## Not in this change

The "Related" point of #467 (Accept on an escalation whose recommendation is a hand step records
the answer and sends the item back before the step is done) is the escalation flow's, a separate item.
