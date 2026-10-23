# Accept holds for a hand step: the escalation's `step` and the two-press Accept (2026-10-23)

Item #533. Seen 7 times on 10-22 (#455 x2, #472 x2, #486, #487, #483): when the orchestrator's
recommendation says "file an item / grep the runner log / edit runner.json, then answer", Accept
recorded the recommendation as the answer at once, the item re-queued, and the agent asked the
same question again — a wasted build each time. The orchestrator had taken to wording
recommendations as the post-step answer and warning in the text, which Accept ignored.

## Decision

**Marked steps, two presses — not "always two presses".** A plain recommendation stays one press;
only an escalation that names a prerequisite hand step holds.

- **`step` on the escalation.** `POST /orchestrator/escalations` takes an optional `step`
  (`OrchEscalateNight`, `data.step`, clipped to 400): the hand step the user must do before the
  recommendation can be the answer. A step needs a recommendation (`missing_field` otherwise);
  the orchestrator's skill says to set it whenever the recommendation starts with something only
  the user can do, and to word the recommendation as the post-step answer.
- **Two-press Accept in Slack.** The message shows "**First:** <step>", says Accept holds, and
  labels the button "Accept — step first". The first press records `orchestrator.accept_pending`
  (`store.AcceptPending`: the same checks as an accept — still needs_you, no trusted-file wait,
  no newer question — and once per escalation) and swaps the buttons for
  **[Done — re-queue] [Other…]**; the item stays in needs_you, its question standing, so no new
  stage. The second press runs `AcceptRecommendation` as before (`answered`, `data.accepted`).
- **The store holds, not just the UI.** `AcceptRecommendation` refuses a step escalation with no
  pending accept (`wrong_stage`), so the API cannot re-queue the item past the step either.
- **The night shift never acts on a step escalation.** `DecideNight` stays ("its recommendation
  needs your hand first"), which also keeps `act_at` off the escalation when it is made; the
  skill says so under the night classes.

Rejected: always two presses (punishes the common plain accept); a new stage between the presses
(the question stands, needs_you already says whose turn it is); holding in the Slack layer only
(the API could still bypass it).

Tests: `TestAcceptStep` (store), `TestNightStepStays` (store), a `hand step` case in
`TestDecideNight` (pipeline), `TestRecommendationAcceptStep` (slack), and the sandbox scenario
`96-orchestrator-accept-step.json`.
