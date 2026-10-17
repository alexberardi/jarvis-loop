# Orchestrate turns: triggers, the queue, the runner's turn and its result (2026-10-17)

Item #73, piece 3 of [phase 8](../phases/08-orchestrator.md). Piece 1 gave the orchestrator its
token and routes ([decision](2026-10-16-orchestrator-token.md)); this is what starts its turns, what
a turn sees, and how its result becomes those routes' calls.

## Decided

- **A third claim kind, `orchestrate`** (`pipeline.WorkOrchestrate`), opted into by
  `ClaimRequest.Kinds` like plan turns, so a runner that predates it never gets one. A queued turn
  beats every item and plan turn for the next free slot, but shares the active-agents limit (a
  running turn is a live lease of stage `orchestrate`), and only one runs at a time
  (`pipeline.ChooseWork`).
- **The queue** (`orch_turns`, migration 0015): at most one queued and one running turn. Each turn
  collects its triggers (`pipeline.Trigger`: reason, item, words, time; the same reason on the same
  item joins, at most 50). A trigger that arrives while a turn runs joins the next one. A turn whose
  lease expires, or that the loop's stop ends, goes back to the queue with its triggers; one past
  the max run time fails.
- **The triggers**, all from loopd's own records (`store.orchHook`, called for every event):
  - an item entering needs_you (`needs_you`), or entering it because its run failed or ran overtime
    (`run_failed`, `pipeline.IsRunFailure`);
  - an expired lease (`lease_expired`);
  - a merge to the loop's own repo (`loop_merged`; `store.SetLoopRepo`, default
    `alexberardi/jarvis-loop`, the sandbox's own in the harness);
  - an item idle in a work stage past its limit (`idle`), checked once a minute: unclaimed, its
    dependencies done, unchanged (`items.updated_at`) for **triage 1 h, build 2 h, review 1 h, land
    1 h** (`pipeline.IdleLimits`). needs_you, held and qa wait for the user, which the other
    triggers and escalations cover. One trigger per wait (`orch_idle` keeps the `updated_at` it
    fired for);
  - a wait the orchestrator met ending (`due`): a retry refused inside its backoff (429
    `too_soon`) sets `orch_state.due_at`, and the turn starts when it ends, not at the next sweep;
  - the user's message to it (`message`, with the words): `store.QueueOrchestrate`. **Not wired
    yet**: loopd's Slack layer records the approver's messages in piece 2 (#72), which calls it.
    The harness's `orchestrator_message` step stands in for it.
- **The sweep**, every 15 minutes while an item is open (`pipeline.OrchSweep`, run by
  `store.OrchTick`, which `Sweep` calls at most once a minute of the store's clock, so on every
  request and loopd's minute ticker). It queues a turn only when the board changed since the last
  turn started: the **fingerprint** is the newest event id, leaving out the orchestrator's own
  actions (actor `orchestrator:*`) and the turns' bookkeeping (kinds `orchestrat*`), so a turn's
  notes do not start the next. Nothing changed: no turn, no model call, nothing recorded.
- **The daily cap**: `orchestrator_turns_per_day` in the control (default 150, 0 to 2000, 0 is
  off; `PUT /control`, the user's: refused on behalf of the user like the limits). Over it, the
  queued turn waits until the day rolls (its triggers keep collecting) and loopd records one
  `orchestrator.capped` event a day.
- **The day** is America/New_York's (the digest's zone): the cap counts turns started on it, and
  the **session rotates** on it. loopd keeps the orchestrator's claude session with the day it
  started (`orch_state`); a turn's claim carries it to resume only on the same day.
- **The snapshot** is loopd's (`GET /orchestrator/snapshot`, any reading token): the cap and
  active agents; the running turn and its triggers; the last finished turn with **how each of its
  actions went** (the refusals come back this way); every open item (stage, priority, deps and
  what it waits on, bounces, seconds since it changed, question, hold reason, PR, head); what is
  working (`store.Working`, with #68's activity); the runners (capabilities, last seen, last
  claim); each triggering item in full (the newest 30 events, evidence list); the newest 30 notes;
  its open escalations (no stage change on the item since, last 48 h); and the user's own events
  since its last turn started.
- **The runner's turn** (`internal/runner/orchestrate.go`), modelled on the plan turns: stage
  `orchestrate` in the runner's `stages`, which **needs `orchestrator_token`** (loopd's
  orchestrator token; not the runner's own token). The token sits in the runner's 0600 config,
  which every agent and verify step already cannot read; the agent never gets it. One short
  `claude -p` (the runner's model, the same as the builders, decision 1) in
  `<work_root>/orchestrator/`, with the plan turns' read-only settings and the guard's write roots
  `out/` and `.loop/scratch/`; no checkout. Its context is `.loop/context/snapshot.json` and
  `repos.md`. A session's first prompt of the day is `runner.OrchStatic` (a stand-in for piece 4's
  skill: who it is, the "never" list, the result contract), then the turn (its triggers, and its
  last turn's actions with their refusals); a resumed turn's prompt is the turn alone. Its time cap
  is `plan_turn_minutes`.
- **The result contract**, `out/result.json`:
  `{"actions": [{"action": "...", "reason": "...", ...}]}`, nothing else. Actions and their fields
  (each needs a non-empty reason; unknown actions or fields refuse the result):

  | action | fields (required in bold) | loopd route |
  |---|---|---|
  | `answer` | **item_id**, **kind**, **text** | `POST /orchestrator/items/{id}/answer` |
  | `retry` | **item_id** | `POST /orchestrator/items/{id}/retry` |
  | `priority` | **item_id**, **priority** | `POST /orchestrator/items/{id}/priority` |
  | `link` | **item_id**, **after** | `POST /orchestrator/items/{id}/link` |
  | `add` | **title**, **signature**, ask, repo, priority, depends_on | `POST /orchestrator/items` |
  | `escalate` | **text**, item_id, recommendation | `POST /orchestrator/escalations` |
  | `agents` | **active_agents** | `POST /orchestrator/agents` |
  | `note` | **text** | `POST /orchestrator/notes` |

  At most 20 actions a turn. A refused result is retried once in the same session with the reason
  (as [refused results](2026-10-16-refused-results.md)); a second refusal fails the turn (its
  session kept). The runner sends the actions one by one with the orchestrator's token; a refusal
  (4xx) is logged and the next goes on. The runner then reports the turn
  (`POST /orchestrate/turns/{id}/result`, runner token: session, error, each action's status and
  code), which loopd keeps on the turn and shows in the next snapshot.
- **The notebook** stays piece 1's events (`note` actions); the snapshot carries the newest notes.
  No file on the runner.

## Left open

- The user-message trigger waits for piece 2's Slack layer (above).
- "An escalation is due" (the spec's "Cost") is built only as the retry backoff's end (`due`).
  Re-pinging an unanswered escalation is the persona's and piece 2's to define.
- For a runner error the snapshot has the failed run's event (its error and last output, as the
  runner reported it), not the runner's local log file: loopd does not have it, and a second
  runner may have run it.
- Item-less turn events (`orchestrate_claimed`, `_done`, `_failed`, `_expired`, `_stopped`,
  `orchestrator.capped`) do not reach Slack; the board's panel (piece 5) and the standup (piece 6)
  read them.
