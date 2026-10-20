# Pacing slows claims, never starves them; the user can ignore warnings and lift the pace

2026-10-20, #316. Amends [usage awareness](2026-10-18-usage-awareness.md).

## Why

On 2026-10-20 the 5h window reset at 04:10. At 04:19 a run came back with the CLI's weekly
`allowed_warning`, so the window went slow. A slow claude takes only P1, size S and triage. Every
ready item was P2 and size M or L, so nothing was claimed from about 04:45 until the user woke at
07:00, with 8 items ready and 6 slots free. The same morning the user had a free usage reset to
spend and no way to tell the loop: `loop budget` could not turn the warning off, because `PaceFor`
treated a warning as pacing whatever the budget said.

## What

- **The fallback.** When a slow pace's filter (P1, size S, triage) passes no item, claude takes the
  first item in the usual claim order (priority, stage, oldest) while fewer than
  `max(1, pace.Agents/3)` claude runs are live. Paused still takes nothing. The flow panel agrees:
  an item only the fallback would take shows as ready while nothing the pace itself takes is ready,
  and as `usage paced` otherwise. Plan turns keep the P1-only rule: a plan turn waits only while the
  user is in the conversation, and the user can lift the pace.
- **`loop budget claude --warnings ignore|pace`** (`Budget.Warnings`, default pace). With ignore,
  the CLI's `allowed_warning` alone never slows claims; the budget's own `pace_at`, the budget and
  limit hits still do.
- **`loop pace normal --until <time|reset>`**, and `loop pace auto` to end it. The lift is
  stored in the control row (`pace_normal_set`, `pace_normal_until`, migration 0035). While it
  holds, the pace is normal: no slowing for warnings or the budget, and no pause at the budget either
  ("lift pacing outright"). A limit hit the CLI reports still pauses claims as before, **and ends
  the lift**: a fixed time ends at that time or at the next hit, whichever comes first, and `reset`
  ends only at the next hit. After a free reset, the user lifts the pace again; the loop never does.
  Setting or clearing the lift writes a note in the orchestrator's notebook (the user is the actor),
  so its turns know the user's call. The board's Usage panel and `loop usage` show
  "normal (pacing lifted until Tue 09:00)" or "... until the next limit hit", and the
  `#loop-digest` note of the change back to normal says the user lifted it.

## Not done

- The orchestrator may still change budgets and the lift on the user's behalf from the user's own
  Slack message (`PUT /control` refuses only the hold rules, limits, run time and its own caps).
  Whether the lift belongs to the user alone is open.
