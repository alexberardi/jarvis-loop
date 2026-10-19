# Feature tags on items (2026-10-18)

Item #198, piece 1 of 4 (feature `smallest-blocks`). An item too big for one run is killed at the
run cap and loses everything, so the user asked for the smallest blocks possible, grouped into
features. Triage sized #198 itself as L, and the user approved splitting it into four pieces: (1) a
feature tag on items, (2) the claim order by feature, (3) a triage `split` outcome, (4) the board
grouped by feature. This decision is piece 1.

## Decision

- **`items.feature`** (migration 0026, `''` for none), `pipeline.Item.Feature` (`feature` in JSON,
  omitted when empty).
- **One spelling:** `pipeline.FeatureSlug` lowercases a name and joins its runs of letters and
  digits with `-` ("Smallest Blocks" is `smallest-blocks`), at most 48 characters. A name the user
  gives (`pipeline.ParseFeature`) that has no letters or digits is refused (`invalid_field`); `""`
  and `none` mean no feature.
- **Set at filing:** `POST /items` and the orchestrator's `POST /orchestrator/items` take
  `feature`; `loop add --feature NAME`.
- **From a plan:** a plan's pieces carry the plan's title as their feature (`PlanItems`).
- **Reopen:** the item a QA reopen files stays in the reopened item's feature.
- **Changed:** `POST /items/{id}/feature` `{"feature": "e2e"}` (`loop feature <id> <name|none>`),
  user-only like the other corrections ([item corrections](2026-10-18-item-corrections.md)),
  through `store.human`: kind `feature`, `data.from`/`data.to`, and a refusal is an event too. Any
  stage, done and closed included: a feature's progress counts its done items.
- **Shown** in `loop show` (`feature:`), in `loop add`'s line, and as a label on the board card.

## Later pieces

- **Claim order (piece 2):** priority, then stage (land and review before build, as today), then
  features already in flight first with a per-feature WIP limit (default 2 in build at once), then
  the longest remaining dependency chain, then age; the claimed event says why.
- **Triage split (piece 3):** a `split` outcome; loopd files the pieces with their dependencies and
  the original's feature, and the original becomes an umbrella that is never built and is done
  when its pieces are.
- **Board by feature (piece 4):** done/total, what the feature waits on, its critical path.

Until piece 3, an item filed too big is split by hand: `loop add --after … --feature …` per piece.
