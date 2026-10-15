# Phase 7: the planning stages

Goal (DESIGN.md, "Planning, before the loop" and the Rollout): a new feature starts as an idea in
`#plan` and leaves as loop items that each carry their part of an approved blueprint, so build
agents implement a plan instead of guessing one. **Proves:** an idea becomes loop-ready items
without a hand-written spec, with the user approving twice (the brief and the blueprint) and
otherwise only talking.

Decisions this phase makes are in `docs/decisions/2026-10-15-planning.md`. The one that departs
from DESIGN.md: briefs and blueprints are **pages on loopd's board**, not Claude Docs pages (why
there).

## The flow

```
#plan top-level message (or /loop plan "idea" [in repo])
  → plan #N, stage brief:      product agent ⇄ user in the thread  → [Approve brief]
  → stage blueprint:           engineering agent ⇄ user            → [Approve blueprint]
  → stage split:               loopd files one item per piece, with dependencies
  → items in triage; plan done when its last item is done (dropped items don't count)
```

- **Any stage → `brief`** when the engineering agent finds the brief infeasible or much bigger
  than expected (its result says `back_to_brief` with why); the product session resumes with that.
- **Drop** works on a plan as on an item (`/loop drop P<n>`, the Drop button on the plan's first
  message): plan → `dropped`; items already filed are untouched.
- Only the approver's Slack ID starts plans, talks in plan threads and approves; other people's
  messages in `#plan` are ignored (logged, no reply), the same rule as `#loop`.

## loopd

### Item dependencies and the plan link (the first piece, useful on its own)

- `items.depends_on` (a list of item ids) and `items.plan_id`, `items.plan_piece` (migration
  `0006_deps_plans.sql`).
- **Claim skips an item whose dependencies are not all `done`** (`qa` counts as not done: the
  piece it depends on isn't verified yet). Pure function in `pipeline.Choose`, table-tested.
- A dependency that is `dropped` or `closed` → the dependent item goes to `needs-you` with
  "#N it depends on was dropped: keep, change or drop this one?". Checked when the dependency
  closes (not on every claim).
- `loop add --after 12,13` and `/loop add … after #12` set dependencies by hand. Cycles and
  unknown ids are refused (400).
- `loop show` and the thread header say "waiting on #12" while blocked; the board shows it on
  the card.

**As built (piece 1, 2026-10-15):** migration `0006_deps_plans.sql` (`depends_on` is a
comma-separated id list like `resources`; `plan_id`, `plan_piece` default 0, no `plans` table yet);
`pipeline.WaitingOn`, `Choose(c, items, deps, live, req)` with the dependencies' stages (unknown
counts as not done), `DependencyCycle`, `ApplyDependencyClosed`. On a dependency's drop or
`not_reproducible` close, in the same transaction, loopd (actor `loopd`, kind `dependency_closed`)
**removes that id from the dependent's `depends_on`** (it can never be done, so "keep" would
otherwise wait forever) and moves an open dependent to `needs_you` with the spec's question; one
already in `needs_you` keeps its own question, and qa/done/closed ones are left alone. Ids are
checked after the insert in the add's transaction, so naming the new item's own id is refused as a
cycle; repeats are dropped. `GET /items/{id}` has `waiting_on`; `loop show` prints
`blocked:  waiting on #12`, `/loop show` and the thread header `· waiting on #12` / `· Waiting on
#12`, the board card a `waiting on #12` label. The header updates on the dependent's own next event
(its claim), not when the dependency finishes. Harness: `scenarios/35-deps.json` (`add` takes
`after`).

### Plans

- Table `plans`: id, title, stage (`brief`, `blueprint`, `split`, `done`, `dropped`), priority,
  repos (one or more slugs, set by the user's words or by the product agent), Slack channel and
  thread ts, the product and engineering session ids, created/updated.
- Table `plan_docs`: plan id, kind (`brief`, `blueprint`), version, markdown, sha256, created.
  Append-only: each agent turn that changes a document adds a version.
- Table `plan_turns`: plan id, the user messages batched into the turn, status (`queued`,
  `running`, `done`, `failed`), lease, result summary. At most one queued or running turn per plan;
  a message that arrives while a turn runs joins the next queued turn.
- **Approvals bind to the document's sha256** (as a merge approval binds to the head sha): an
  Approve button carries the version it was posted for, and a newer version makes it stale
  ("the brief changed; approve v3 instead"). Echo, Cancel and the 2-minute grace as for merges.
- Approving the blueprint runs the **hand-off** in one transaction: one item per piece
  (`source=plan`, the plan's priority, `plan_id`, `plan_piece`, `depends_on` mapped from piece
  indices to the new item ids, the piece's section of the blueprint as the ask's tail), plan →
  `split`. Items are posted to `#loop` as usual, each linking back to the plan thread and page.

### API

- `POST /plans` (user), `GET /plans`, `GET /plans/{id}` (both), `GET /plans/{id}/docs/{kind}`
  (both; latest, or `?v=`).
- Runner: `POST /claim` gains a turn kind: a claim may return a plan turn instead of an item
  (`ClaimRequest.Kinds` includes `plan` when the runner's config `stages` has `plan`). Plan turns
  share the active-agents limit with items; priority = the plan's, and a waiting user beats a
  queued item of the same priority (the user is in the conversation).
- Runner: `POST /plans/{id}/turns/{turn}/result` with the agent's result (below) and evidence.
- User: `POST /plans/{id}/approve` (kind, sha256), `POST /plans/{id}/drop`.

## Runner: conversational turns

- A plan turn runs **one short agent** in a stable directory `<work_root>/plan-<id>/`, so
  `claude -p --resume <session>` finds the session (sessions are stored per working directory).
  The first turn of each persona starts a session; loopd stores its id; later turns resume it,
  so the conversation is never replayed (DESIGN.md, prompt caching).
- The directory holds read-only worktrees of the plan's repos at `origin/main`, refreshed before
  each turn (fetch + reset; the agent cannot write them: the guard's read-only rule from review,
  `deny_write` for the whole tree except `out/`). No verify steps, no harness, no GitHub.
- The turn's prompt is the batched Slack messages, verbatim; when the engineering agent sent the
  plan back, the product turn's prompt starts with its reason. The first turn of each persona also
  gets the plan's documents and the configured repos list under `.loop/context/`.
- **Personas** are skills, like triage/build/review: `skills/product/SKILL.md`,
  `skills/engineering/SKILL.md`. Each has a byte-identical system prompt and tool set across plans
  (a per-persona prefix golden, as phase 5 did per stage).
  - Product: asks what the user wants, who it's for, success criteria, out of scope; reads code
    only to ground what exists; writes the brief to `out/brief.md` when it has one, revises it on
    request, and says when it thinks it's ready to approve.
  - Engineering: starts after the brief is approved, in its own session, from the brief and the
    code. Writes `out/blueprint.md`: approach, affected modules, interfaces, test plan, harness
    verification plan (scenarios, shots), risks, and the pieces. Each piece: title, repo, size,
    depends on (piece numbers), its blueprint section, how it's verified. Pieces are loop-sized: one
    PR each, a few hours of agent work at most; the agent splits further rather than make a big one.
- **Result** (`out/result.json`): `reply` (the Slack message, ≤ 3000 chars, no logs or
  transcripts), optional `brief` or `blueprint` (path in `out/`), optional `ready: true` (post the
  Approve button), optional `back_to_brief` (engineering only, with why), optional `repos` (product
  only, configured slugs). The runner validates it (unknown repo, a blueprint whose pieces don't
  parse or whose dependencies loop → the turn fails with that reason and the reply says so).
- Limits: `plan_turn_minutes` (config, default 15), same overtime handling as items. A failed turn
  posts "that turn failed: <reason>; reply to try again" and keeps the session.

## Slack

- The bot joins `#plan` (`C0C8Y132YRM`; already a member). A top-level message there from the
  approver starts a plan (title = first line, priority words and `in <repo>` as in `#loop`); the
  bot answers in the thread with the plan number and queues the first product turn.
- Replies in a plan thread queue a turn. While a turn runs, the bot adds a ⏳ reaction to the
  message; ✅ when the reply is posted.
- When a turn says `ready`, the reply ends with **Approve brief v2** / **Approve blueprint v1**
  buttons and a link to the document page. Approving the blueprint lists the filed items in the
  thread.
- `/loop plans` lists open plans; `/loop drop P<n>` drops one.

## The board

- The **Planning** column shows open plans (stage, last activity, whose turn it is: "you" or
  "agent").
- `GET /board/plans/{id}`: the plan's documents rendered as HTML (latest, with older versions
  linked), its turns, and its items with their stages. Markdown is rendered server-side with an
  allow-list (headings, lists, tables, code, links; no raw HTML), so the CSP stays as it is.
- Cards of items from a plan show the plan's number, linking to this page.

## Into the repo

The first item of a plan to reach build gets `.loop/context/plan.md` (brief + blueprint) and the
build skill tells it to commit it as `docs/plans/<plan-slug>.md` in its PR; later items get the
same file and a note that it is already committed. Review checks the piece against the blueprint's
section and its verification plan; triage checks the blueprint still matches the code (a mismatch
→ `question` on the item, not a silent rewrite).

## Harness (loop-sandbox scenarios, test-first)

1. Dependencies: B depends on A → B not claimed until A is done; A dropped → B to needs-you.
2. `#plan` message → plan, product turn (fake claude writes a brief, `ready`) → Approve brief →
   engineering turn (blueprint with 3 pieces, 2 depending on 1) → Approve → 3 items with the right
   dependencies; only piece 1 is claimable.
3. A reply arriving while a turn runs joins the next turn (one turn, both messages).
4. A stale Approve (brief changed after the button was posted) is refused with the newer version.
5. `back_to_brief` → the product session resumes with the reason; the blueprint stage restarts
   after the next approval.
6. Not the approver: a message in `#plan` from another user starts nothing.
7. Resume: the second turn of a persona passes `--resume <id>` and the prefix golden holds.
8. A blueprint with a dependency cycle → the turn fails, nothing is filed.

## Pieces (filed as loop items, in this order)

1. **Item dependencies and plan link** (loopd, CLI, Slack `after #N`, board "waiting on").
2. **Plans in loopd** (tables, API, approvals bound to sha256, hand-off), depends on 1.
3. **Plan turns in the runner + the two persona skills**, depends on 2.
4. **`#plan` in Slack** (threads, buttons, reactions, `/loop plans`), depends on 2.
5. **Board: Planning column and plan pages**, depends on 2.

All five change loopd's pipeline, store, API or the Slack approver check, so each is held for the
user at land.

## Real run

After the five land and the runner's config has stage `plan`: the user posts one small real
feature idea in `#plan` and takes it through brief, blueprint and hand-off; its first item goes
through the loop as usual.
