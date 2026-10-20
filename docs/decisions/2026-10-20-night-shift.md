# The night shift: loopd acts on the orchestrator's recommendation while the user is away (2026-10-20)

Item #311. The user, 2026-10-20 00:1x: "can we set up some sort of recurring task for the
orchestrator to make sure it keeps items moving?" The recurring part existed (the 15-minute sweep,
the idle, needs_you, run_failed and lease_expired triggers: 2026-10-17-orchestrate-turns.md). What
stalled items overnight was authority: in one day the orchestrator escalated 19 of its 80 actions,
among them a verify regression it judged unrelated to the PR (#298), a third bounce, a scope choice
where it already recommended doc-only, and its 5-a-day ticket cap. Each waited for the user until
morning.

## Decision

**A night shift on the control row.** `night_shift`: `off` (the default), `on`, or `auto` = 23:00 to
08:00 New York (`store.OrchDayZone`). In auto mode the approver's `@orchestrator night` or
`@orchestrator morning` (a mention or DM with only that word) turns it on or off until the next
23:00 or 08:00; loopd answers it itself, without a turn (`orchestrator.night_override`). A "night"
in the day runs on through the night to 08:00; a "morning" before 08:00 lasts the day. The user
turns classes off one by one (`night_classes_off`). `loop night [off|on|auto] [--classes-off …|none]`
sets and shows it; `GET /night` reads it.

**The orchestrator names a class; loopd checks it and takes the class's one action.** An
`escalate` may carry `class` (and, for a scope choice, `deferred`). An escalation made while the
night shift is on (and the class is on) is acted on once it has waited 15 minutes
(`pipeline.NightGrace`; the sweep runs once a minute, so up to a minute later), unless the user
answered, the item moved on or asks something new, or the approver spoke to the orchestrator in its
thread. Its Slack message says when ("Night shift: I act on this recommendation at 02:15 unless you
answer first.") only when loopd would act.

| Class | loopd's check | Action | Limit | Undo |
|---|---|---|---|---|
| `verify_flake` | the item is with the user after the runner's verify failed | rebuild with notes to re-run verify | once per head sha | `loop rebuild N --notes "…"` |
| `bounce_limit` | the item is with the user after a review bounce | rebuild with the latest review notes (the question) | once per item per night (23:00 to 23:00) | `loop rebuild N --notes "…"` |
| `scope_choice` | a `deferred` part is given | answer with the recommendation; file `deferred` as a P3 item (`Deferred from #N: …`) | none beyond the item moving on | `loop drop <P3>`, then `loop rebuild N --notes "…"` |
| `ticket_limit` | the ticket is against the loop's own repo (`jarvis-loop`) | the 24-hour cap is 10 instead of 5 at night | 10 | `loop drop <ticket>` |

"The PR's paths cannot reach the failing scenario" and "the review notes are concrete must-fix
items" are the orchestrator's judgement, recorded as its reason; loopd checks only what it can.

**Never at night, whatever the class** (`pipeline.DecideNight`, recorded once as
`orchestrator.night_stayed` with why, and said in the thread): an item on the hold list (risk
`hold`: triage's hold, a PR touching a hold rule, a trusted-file change), an approval, an item
waiting on a trusted-file OK, an escalation made while the night shift was off, and a
recommendation, deferred part or rebuild notes that name a never-list entry (merges and approvals,
credentials, auth, signing, releases and tags, prod and deploys, deleting work, limits and budgets,
CI and workflow files, test-weakening, hand fixes on a runner or a mirror). The word check is loose
on purpose: a false hit only leaves the escalation to the user. The night shift never merges,
approves, changes the control or a limit: its only actions are a rebuild, an answer and a P3 item.

**Visibility.** Every action is an `orchestrator.night_action` event (class, action, reason, undo,
escalation, head sha; actor `orchestrator:night-shift`), posted in the item's thread as
"Night shift: <action> — <reason>" with its undo. Its transition (`rebuild`, `answered`) carries
`night_shift` and is not posted again. The 09:00 standup lists the night's actions at the top with
a one-line undo each; the board's Orchestrator panel lists them with the orchestrator's other
actions. The deferred P3 item is not an `orchestrator.added`, so it does not count against the
orchestrator's cap.

## Not done here

- A board toggle for the mode: the CLI and the API (`PUT /control`) set it.
- The orchestrator persona of the harness (`internal/fakeclaude`) does not choose classes; the
  scenario (`82-night-shift-verify-flake`) posts the classed escalation as the orchestrator.
