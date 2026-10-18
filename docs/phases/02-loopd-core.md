# Phase 2: loopd core

Goal (DESIGN.md, Rollout 2): items, events, the stage rules, priority, the active-agents number,
leases, the API and the `loop` CLI, on the dev box, with stages moved by hand (or by a test runner
script). No Slack, no agents yet. **Proves:** the state machine and limits.

## Layout

```
go.mod                       module github.com/alexberardi/jarvis-loop, go 1.27
cmd/loopd/                   the server: flags, wiring, graceful shutdown
cmd/loop/                    the CLI
internal/pipeline/           PURE rules: stages, outcomes, transitions, claim choice, limits (no I/O)
internal/store/              SQLite (modernc.org/sqlite, CGO off), embedded versioned migrations
internal/api/                HTTP handlers, auth, JSON shapes
internal/web/                the status page + active-agents slider (server-rendered, no JS build)
```

Dependencies: the standard library and `modernc.org/sqlite`. Nothing else without a reason.

## Stages and outcomes

An item is in exactly one **stage**, and in a work stage it is either **waiting** or **claimed**
(a live lease). Display names follow DESIGN.md:

| stage | waiting shows as | claimed shows as |
| --- | --- | --- |
| `triage` | intake | triage |
| `build` | ready | build |
| `review` | review (queued) | review |
| `land` | land (queued) | land |
| `needs_you` | needs-you | — (never claimed) |
| `held` | held | — (never claimed) |
| `done`, `closed` | done / closed | — |

A new item starts at `triage`, waiting. A stage ends only through `POST /items/{id}/advance` by the
lease holder, with an **outcome**:

| from | outcome | required fields | to |
| --- | --- | --- | --- |
| triage | `reproduced` | area, size (`S`/`M`/`L`), risk (`normal`/`hold`), resources (subset of `gpu`, `voice`, `emulator`, `macos`), ≥1 evidence | build |
| triage | `not_reproducible` | ≥1 evidence, summary | closed |
| triage, build | `question` | question text | needs_you (return stage = from) |
| build | `pr_opened` | repo, pr number, branch, head sha, **changed_paths**, ≥1 evidence; optional `needs_review` (`true` → hold) | review; risk recomputed (see Hold risk) |
| review | `approved` | summary, head sha (must equal the item's head sha) | land |
| review | `bounce` | notes | build if this is the 1st bounce; needs_you on the 2nd (bounce count ≥ 2), with the notes as the question |
| land | `merged` | merge sha, **head sha** (the PR head that was merged), **changed_paths** | done; risk recomputed on those paths, and a hold item needs `approval.head_sha == head sha`, else 409 `approval_required` (stays in land) |
| land | `hold` | hold reason, **head sha**, **changed_paths** | held; the item's head becomes this sha and an approval for another head is void |
| land | `back_to_build` | reason | build |
| any work stage | `usage_limit` | error text, reset text if any | same stage, waiting; **active agents := 0**; event `usage_limit` |
| any work stage | `failed` | error text | needs_you, with the error as the question |

**Hold risk** is computed by loopd, not trusted from an agent (review of PR #1;
`docs/decisions/2026-10-14-hold-rules.md`). `changed_paths` is the PR's files in `git diff
--name-status` form (`M internal/x.go`, `D foo_test.go`, `R old new`). Control holds the **hold
rules** (`hold_rules`: `{name, glob, statuses?}`, glob with `**` and `*`; set with `PUT /control`,
persisted; defaults are jarvis-server's hold list). Risk only ratchets up: it is `hold` if triage said
`hold`, or any rule matches a changed path, or `needs_review` was set, or land held it, and stays
`hold` after that. Each reason (`auth: M internal/modules/auth/x.go`, `triage: risk hold`, `marked
needs review`, `land: <reason>`) is added to the item's `hold_reason`; the matches are on the event.

Human transitions (user principal only):

- `POST /items/{id}/answer {text}` on `needs_you` → the return stage, waiting.
- `POST /items/{id}/approve {head_sha, words?}` on `held` → `land`, waiting, with an approval record
  `{by, words, head_sha, at}`. Refused if `head_sha` ≠ the item's head sha (an approval is for the
  commit the user saw). When a later `pr_opened`/push changes the head sha, the approval is void.
- `POST /items/{id}/hold {words?}` on `held` → stays held; recorded.
- `PATCH /items/{id} {priority?, title?}` from any stage except done/closed.

Every transition and every refusal-worthy action writes an **event** (append-only): `{item, at,
actor, kind, from, to, outcome, data(json)}`. Nothing else ever deletes or rewrites history.

**Review independence:** the claim carries an `agent` id. An item remembers its builder's agent id
(from the `pr_opened` advance). A review claim by that same agent id must not be granted for that
item (the server skips it when choosing).

## Claims

`POST /claim {runner, agent, capabilities[]}` returns `{item, stage, lease{id, expires_at}, brief}`
or `204` when there is nothing to do. Rules, all in `internal/pipeline` as pure functions over a
snapshot so they are table-testable:

1. **Control:** if `stopped` or `active_agents == 0` → nothing. If live leases ≥ `active_agents` →
   nothing.
2. **Eligible items:** waiting, in a work stage, whose `resources` ⊆ the runner's capabilities
   (triage items have no resources yet → any runner).
3. **Per-resource limits** (from control, defaults `gpu 2, voice 1, emulator 1`): an item needing a
   resource at its limit (counting live leases) is skipped.
4. **Per-stage limits** (defaults `triage 1, review 2, land 3` (land 1 until #213); build is limited only by 1).
5. **Review independence** (above).
6. **Order:** priority (P1 first), then stage (land, review, build, triage: finish work in flight
   before starting new), then oldest `created_at`, then id.

`brief` for now is just the stage name and the item; the stage skills come in phase 4.

## Leases

- Default TTL 5 minutes, extended by `POST /leases/{id}/heartbeat`; the reply is `{stop: bool}`
  (true when control is `stopped`, or the lease was revoked).
- **Max run time** (control, default 45 minutes from claim): a lease past it is ended; the item goes
  to `needs_you` with "run exceeded 45 minutes" and the last heartbeat note.
- An **expired** lease (no heartbeat within TTL) is ended; the item returns to waiting in its stage;
  event `lease_expired`.
- Expiry is evaluated on every request and by a ticker (1 min). **All time comes from an injected
  clock**; tests never sleep.
- `advance` and `evidence` require the caller to hold the item's live lease.

## Control

`GET /control` and `PUT /control {active_agents?, stopped?, max_run_minutes?, limits?}` (user only).
`active_agents` is 0–6, default 4. `stopped: true` means no claims and heartbeats answer
`stop: true`; `false` resumes. Every change is an event (`control`) with the actor.

## Evidence

`POST /items/{id}/evidence` (multipart, field `file`, optional `label`), lease holder only, ≤ 20 MB
each. Stored under `<data>/evidence/<item id>/<n>-<sanitized name>`; recorded with sha256, size,
content type. `GET /items/{id}/evidence/{n}` serves it (any authenticated principal).

## Items and the board

- `POST /items {title, ask, repo, priority?, source?}`: priority `P1`–`P5` (default `P3`), source
  one of `user`, `finding`, `nightly`, `scout` (default `user`; scout items default to `P4`).
  Runners and the user may add items.
- `GET /items?stage=&priority=` → the board: id, title, priority, display stage, claimed by, age.
- `GET /items/{id}` → the item, its events, its evidence list, its approval if any.

## Auth

`--tokens FILE` (0600, `token<TAB>kind<TAB>name` per line, kind `runner` or `user`). Every request
carries `Authorization: Bearer <token>`. Runner-only: claim, heartbeat, advance, evidence upload.
User-only: control PUT, answer, approve, hold. Both: items add/read, PATCH priority, evidence read.
Compare tokens in constant time. Unknown token → 401; wrong kind → 403. Never log tokens.

## Server

`loopd --addr 127.0.0.1:7780 --data ~/.local/share/jarvis-loop --tokens ~/.config/jarvis-loop/tokens`.
Default bind is loopback (LAN binding is a deliberate flag later). SQLite in WAL mode, one writer.
Graceful shutdown on SIGINT/SIGTERM. JSON errors `{"error": "...", "code": "..."}`. Structured logs
with `log/slog`.

## Web page

`GET /` (user token as a cookie set by `GET /login?token=` once, or basic auth with the token; pick
the simpler secure option and document it): the board grouped by display stage, and the
active-agents slider (a form POST to `/control`, no JavaScript build). Small and plain.

## CLI (`loop`)

`LOOP_URL` (default `http://127.0.0.1:7780`) and `LOOP_TOKEN`. Commands: `add "<title>" [--ask ..]
[--repo ..] [--priority P2]`, `status`, `show <id>`, `answer <id> "<text>"`, `approve <id> --sha
<sha> [--words ..]`, `hold <id>`, `priority <id> P1`, `agents <n>`, `stop`, `resume`, and for
testing the machine by hand: `claim --runner R --agent A --cap gpu,voice`, `heartbeat <lease>`,
`advance <id> <outcome> [--field k=v ...] [--evidence FILE ...]`.

## Tests (TDD: write them first)

- `internal/pipeline`: a table test per transition row above (allowed, refused with the right
  error, required fields), bounce counting, review independence, approval/head-sha binding,
  usage_limit setting active agents to 0, and claim choice: control gates, resources ⊆ caps,
  per-resource and per-stage limits, the order (priority, stage, age, id).
- `internal/store`: migrations from empty, round-trips, events append-only, concurrent claims never
  hand out the same item twice (run under `-race` with goroutines).
- `internal/api`: httptest end to end with a fake clock: auth 401/403, a full item walk
  (add → claim triage → reproduced → claim build → pr_opened → review by a *different* agent →
  approved → land → hold → approve with the right sha → land → merged → done), lease expiry,
  max run time, stop via heartbeat, evidence upload/download.
- `cmd/loop`: a smoke test against an httptest server.
- `CGO_ENABLED=1 go test -race ./...` green; `CGO_ENABLED=0 go build ./...` and vet for
  linux/darwin.
