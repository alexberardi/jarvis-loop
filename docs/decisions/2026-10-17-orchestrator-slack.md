# The @orchestrator Slack app (2026-10-17)

Item #72, piece 2 of [phase 8](../phases/08-orchestrator.md) ("Asking the user", "Its own persona
in Slack"). Piece 1 ([decision](2026-10-16-orchestrator-token.md)) gave the orchestrator its token,
its actions and acting for the user on a recorded approver message; this piece is the Slack side
that records those messages and posts what the orchestrator says. Its turns are piece 3 (#73).

## Decided

- **A second app, a second socket.** loopd reads the `@orchestrator` app's tokens from
  `--orchestrator-slack-env`, by default `orchestrator-slack.env` next to the `--slack-env` file
  (the path in the manifest's header): 0600, the same `SLACK_BOT_TOKEN` / `SLACK_APP_TOKEN` format
  (`slack.LoadEnv`). Absent (or `off`): the app is off and the loop bot is exactly as before. An
  explicit file must exist; tokens equal to the loop bot's are refused (it is a second app, never
  the same one twice). `loopd.StartSlack` runs `slack.Orchestrator` and its own `slack.Socket` beside
  the bot. Its client may post in `#loop`, `#loop-digest`, `#plan` and DMs (`slack.DMs`), nowhere
  else. It learns its own bot user id from `auth.test` (retried on each tick until it answers).
  The runner's verify fence masks the new file, as it does `slack.env`.
- **What it records.** On its socket, a DM to it, or a message mentioning it in one of the loop's
  three channels, from anyone, becomes in one transaction a `slack_messages` row (what
  `X-Loop-On-Behalf` names, piece 1) and an **`orchestrator.message`** event (no item; `data`:
  `channel`, `ts`, `thread_ts`, `user`, `text` without the mentions, `approver`, and `item_id`
  when it is in an item's `#loop` thread). Slack's `app_mention` copy of a channel message is
  ignored (the `message` event is the one recorded), and a message seen twice records nothing. It
  adds 👀 to a message it recorded, so the user sees it was heard before piece 3's turns answer.
- **The loop bot leaves those messages alone.** A message that mentions the orchestrator's bot
  user is never an answer, a reply approval, a held note, a plan message or an `@jarvis-loop` add:
  "@orchestrator answer #46: …" or "@orchestrator approve #28" in a thread is the orchestrator's
  only. Until the orchestrator's `auth.test` has answered, the loop bot cannot tell its mention, so
  a message that mentions anyone but the loop bot itself (its own id from its own `auth.test`,
  asked then) is left alone too, and any mention while that is unknown as well: never misread as
  an approval, a trust OK or an answer. Without the app, nothing changes.
- **The hand-off to piece 3:**

  | Route (orchestrator token) | What |
  |---|---|
  | `GET /orchestrator/messages?since=<event id>&limit=` | the `orchestrator.message` events after that id, oldest first (default and cap 200; any reading token, so loopd's own tokens can read DM text). A turn's trigger and its "user's replies since its last turn" |
  | `POST /orchestrator/replies` `{channel, thread_ts, text}` | an `orchestrator.reply` event, which the app posts (escaped) as itself; `channel` must be `#loop`, `#loop-digest`, `#plan` or a DM (400 otherwise) |
  | `POST /orchestrator/items/{id}/approval` `{reason}` | asked to approve, it cannot: an `orchestrator.approval_asked` event on an item **held for approval** or **waiting on the approver's OK of trusted files** (#70; 409 `wrong_stage` otherwise, a `refused` event), and the loop bot posts the item's approval message again in its thread, with its sha-bound buttons and the reason ("Orchestrator: approvals are yours, so here is its approval message again — …"); for trusted files, the item's trust message with its Trust button bound to the refused commit (`data.trust`, the refusal's `trust_sha` / `trusted_paths` / `trusted_diffstat`; "Orchestrator: trusted-file OKs are yours, so here is its trust message again — …"). Nothing is approved or trusted |

  Acting for the user stays as piece 1 built it: the turn calls the user's route with
  `X-Loop-On-Behalf: <channel>/<ts>` of the recorded message.
- **Recommendations.** The app posts each `orchestrator.escalated` event (the orchestrator's, and
  loopd's own on a third failure), the loop bot no longer does: mentioning the approver, "**#N
  title** needs you: <text>", "**Recommend:** … _Because_ <reason>", and **[Accept] [Other…]**.
  Accept is there only when it can be the answer: the item is in needs_you and there is a
  recommendation; never for an approval (the item is held: it says to use the approval
  message's button), an item waiting on a trusted-file OK (the recommendation's words are not the
  OK verify needs: it says to use the trust message's button, and `AcceptRecommendation` refuses
  it too), or an escalation about no item.
  - **Accept** is the approver's (`IsApprover`, as approvals; anyone else gets the usual
    ephemeral and a `refused` event). It runs `store.AcceptRecommendation`: the recommendation is
    the approver's answer (`answered`, actor `slack:<approver>`, `data.accepted` = the escalation's
    event id), refused if the item has left needs_you or asks something new since the escalation
    (a later needs-you entry), so an old Accept never answers a newer question. The buttons give
    way to "Accepted by you." (the message as Slack sends it with the press, so no state is kept);
    the thread line reads "Answered by you, accepting the orchestrator's recommendation: …".
  - **Other…** only tells the presser how: reply in the item's thread (any reply there is the
    answer, the existing path), or mention / DM the orchestrator.
- **Batching.** An escalation waits `OrchestratorBatch` (default 2 minutes, from the event's time)
  for others; the batch is flushed by the app's tick. One goes in its item's thread (or `#loop`
  when it is about none). Several go in one `#loop` message (one mention of the approver, a
  section and its buttons per item, at most 10 per message) plus "Orchestrator: asked you about
  this in #loop, with N other items." in each item's thread. No quiet hours (decision 2). With the
  store's one-per-10-minutes limit, a batch of several is an approval escalation or loopd's own
  plus one more.
- **"Orchestrator: …" lines.** An event whose actor is `orchestrator:*` reads "Orchestrator:
  <what> — <reason>." in the item's thread: answered (with its kind), retried (failure n),
  priority, now waits on #N, filed this item, saw this again; a refusal reads "Orchestrator: tried
  to <action>; refused: …". An action it made for the user (`data.via`) reads "… by you via
  orchestrator". Lowering the active agents (`ping: true`) also pings the approver in
  `#loop-digest` from the app. With the app off, the loop bot posts an escalation as a plain
  message (mention, text, recommendation; no buttons).
- **Harness.** The fake Slack holds a second app (`slackfake.AddApp`: its tokens, bot user and
  socket; mentions reach both apps, DMs only it, a press goes to the app that posted the message);
  the sandbox stack runs it always. Scenario steps `tell_orchestrator` and `orchestrator` with
  `as` send real messages through it, so piece 1's scenario now goes the real path. New:
  `58-orchestrator-accept`, `59-orchestrator-acts-for-user`.

## Left open

- **The batch is in memory.** A loopd restart inside the 2-minute window loses that ping (the
  escalation stays an event; the orchestrator's turn sees its open escalations and can escalate
  again). Persisting it is a small follow-up if it bites.
- **Replies are not tied to a message.** `POST /orchestrator/replies` may post in any allowed
  channel or DM thread; piece 3's turn decides where, from the messages it read.
- **A live two-app Socket Mode** is checked only after the user creates the app from the manifest
  and drops its tokens in place; the harness's fake is the evidence until then.
