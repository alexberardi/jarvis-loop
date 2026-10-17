# The orchestrator's token and its actions (2026-10-16)

Item #71, piece 1 of [phase 8](../phases/08-orchestrator.md). loopd gets a token kind for the
orchestrator and enforces "What it may do" itself, so the orchestrator's skill (piece 4) only has
to know the limits, never be trusted with them.

## Decided

- **Token kind `orchestrator`** in the tokens file (`token<TAB>orchestrator<TAB>name`); its
  actor is `orchestrator:<name>`. It reads (`GET /items`, `/items/{id}`, evidence, plans,
  `/control`, its notebook) and acts only through its own routes. Every user and runner write
  route refuses it with 403 `forbidden`: approve, drop, hold, rebuild, answer, qa, verified,
  reopen, `PATCH /items`, `POST /items`, `PUT /control` (config and hold rules), plans, claims and
  leases. A refusal on an item's route is recorded as a `refused` event on the item.
- **Its routes**, each requiring a non-empty `reason` that its event records:

  | Route | Event | Limit loopd enforces |
  |---|---|---|
  | `POST /orchestrator/items/{id}/answer` `{kind, text}` | `answered` (`answer_kind`, `head_sha`) | `kind` one of `history`, `harness`, `bounce`, `red_ci`, `loop`; at most 2 per item per head sha; not the same text (case and spaces ignored) as its previous answer on the item; `bounce` once per item; an item stuck on a run failure is refused (that is a retry) |
  | `POST /orchestrator/items/{id}/retry` | `retried` (`failure`, `head_sha`) | only an item whose last entry into needs_you was a failed run or a run past the time limit; the n-th failure on a head waits 2 min (n=1) or 10 min (n=2) after the failure (429 `too_soon`, with the time); the third is refused (409 `limit`) and loopd records `orchestrator.escalated` itself |
  | `POST /orchestrator/items/{id}/priority` `{priority}` | `patched` | P1–P5, P1 included (decision 3) |
  | `POST /orchestrator/items` `{title, ask, repo, priority, depends_on, signature}` | `created` + `orchestrator.added` | `signature` required, normalised (urls, paths, shas, numbers removed; lower case); an open item filed with the same signature is returned (200, `duplicate: true`) with an `orchestrator.seen_again` event; otherwise at most 5 in a rolling 24 hours (409 `limit`). Source `finding` |
  | `POST /orchestrator/items/{id}/link` `{after}` | `linked` | both items open (not done or closed), not itself, not already linked, no cycle |
  | `POST /orchestrator/escalations` `{item_id, text, recommendation}` | `orchestrator.escalated` | one per 10 minutes (429 `too_soon`), except about an item held for approval |
  | `POST /orchestrator/agents` `{active_agents}` | `control` with `ping: true` | strictly lower than the current number, so never above the user's |
  | `POST /orchestrator/notes` `{text}`, `GET /orchestrator/notes?limit=` | `orchestrator.note` | — |

  The notebook is these events (no file or table): the board's panel (piece 5) reads them.
- **Acting for the user.** An orchestrator request with `X-Loop-On-Behalf: <channel>/<ts>` runs as
  the approver: actor `slack:<approver>` (the same as their own Slack actions), and every event it
  writes carries `data.via = orchestrator:<name>` (`store.WithVia`). loopd checks, in one
  transaction:
  - the message is one loopd's Slack layer recorded (`slack_messages`, migration 0014,
    `store.RecordSlackMessage`), so a made-up ts is refused (403 `not_approver`);
  - its author is `slack.Approver`, so someone else's message is refused (403 `not_approver`);
  - it is at most 10 minutes old (403 `not_approver`);
  - it was not used before for the same action on the same target (`slack_message_uses`; 409
    `message_used`). One message may act on several items ("drop #61 and #63"), once each.
  - the route is on an **allowlist** (`onBehalfRoutes`): answer, hold, rebuild, drop, qa,
    verified, reopen, `PATCH /items`, `POST /items`, `PUT /control` (only `stopped` and
    `active_agents`, "pause the loop": a body that sets `hold_rules`, `limits` or
    `max_run_minutes` is refused, 403 `not_on_behalf`, with a refused event, and the message is
    not spent), plan create, message and drop. Merge approvals, plan approvals and trusted-file OKs (#70's route, when it lands) are not
    on it, so they are refused (403 `not_on_behalf`) even with the approver's message; so is any
    route added later until someone puts it there.

  The message's words are not matched to the action: the orchestrator reads them. The window, the
  one-use rule and the events (with `via`) bound and show what it did with them.
- **Recording the approver's messages** is piece 2's (the `@orchestrator` app). Until then nothing
  writes `slack_messages` outside tests and the harness, so acting for the user is off in practice.

## Left open

- **The 30-minute backoff.** The spec lists 2, 10, 30 minutes and escalates the third failure on a
  head; with both, the 30 is never waited for. Built as the stricter reading (two retries, the
  third failure goes to the user); `pipeline.OrchRetryBackoff` keeps the 30 for when the user
  says otherwise.
- Slack wording ("Orchestrator: …", "via orchestrator") is piece 2's; today the new events show as
  their kind ("retried.") and refusals as "Refused … from orchestrator:loop".
