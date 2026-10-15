# Choices made where the phase 3 spec left room (Slack)

2026-10-14 · phase 3 (Slack). Each is enforced by a test in `internal/slack` or `internal/store`.

**The one new dependency: `github.com/coder/websocket`.** Socket Mode needs a WebSocket client and
the standard library has none. coder/websocket (formerly nhooyr.io/websocket) is small, has no
dependencies of its own, is pure Go (fine with `CGO_ENABLED=0`), takes a `context.Context` on every
call, and allows concurrent writes, so a slow `/loop` answer can be acked from its own goroutine.
gorilla/websocket would also work but needs a write lock of our own and has no context support.
No Slack SDK: the Web API we use is five JSON calls, and our own client is what enforces the
channel allowlist below.

**Where the bot may speak.** The Web API client is built with the two channels (`#loop`,
`#loop-digest`) and refuses, without calling Slack, any post, update, ephemeral or pin elsewhere
(`ErrChannelNotAllowed`). So:

- An `@jarvis-loop` mention in another channel adds the item (its thread is in `#loop`) but gets no
  reply there; a refusal there is recorded, not answered.
- `/loop` answers ride on the Socket Mode ack, which Slack shows only to the person who typed it
  (that is not a bot post). An answer slower than 2 s is acked "working…" and then sent with
  `chat.postEphemeral`, only if the command came from one of the two channels. `response_url` is
  not used, so there is no second outbound path to vet. loopd's commands are local SQLite calls and
  take milliseconds.

**Fan-out.** The store collects the events a transaction writes and hands them to subscribers after
`COMMIT`, in commit order (an emit lock is taken before the writer lock is released). Rolled-back
events are never emitted. The Slack subscriber only queues (4,096 deep; a full queue drops and
logs). One goroutine posts them. Each Slack call is retried at most 3 times, and only on transient
failures (HTTP 5xx, network errors, rate limits after the client has waited out `Retry-After` up to 3
times). An `ok:false` error such as `channel_not_found` is not retried. Failures are logged with
Slack's error code and never with a token.

**Threads.** An item with no parent message gets one when its next event arrives (for example after
a failed post, or for items made before Slack was on). Parents are updated with `chat.update` after
every reply, showing the item as it is now. Replies never name runners or agents (runner names can
be host names) and never name or quote evidence (just "evidence attached (n)"). User and agent text
is escaped (`&`, `<`, `>`), so it can never become a mention, `<!channel>` or a link. Long text is
cut with "… (full text on loopd: loop show N)". With `--slack-link URL`, it is a link to the item on
loopd instead, off by default because loopd listens on loopback and a phone cannot open that.

**Who.** Slack actions are recorded with actor `slack:<member id>`. The approver (`U0C9AFPAMD4`) and
the two channels are constants in `internal/slack`, not flags, so changing who may approve is a
reviewed code change. Anyone else gets an ephemeral "only the approver can do that" (in the two
channels) and a `refused` event: on the item when there is one, otherwise with no item (control,
add, usage).

**Reply approvals.**

- Classifier, stricter than the spec's wording in three ways, all toward "not an approval": a
  negation anywhere in the reply counts as "nearby" (so "yes, no problem" is not consent); a reply
  that opens like a question ("is this fine") is a question even without a `?`; and hyphenated words
  are one word ("fine-tune" is not "fine"). Words ending in `n't` are negations, and `nope`, `nah`,
  `never`, `cancel`, `holding` join the spec's list.
- The approval is bound to the item's head sha when the reply arrives, not when it applies.
- The 2-minute window lives in memory. A restart within the window drops it, which errs on the safe
  side: nothing merges that the user did not see applied. The echo is not posted → no pending
  approval (the user would have no Cancel button).
- One pending approval per item. Pressing **Approve merge** or **Keep holding** cancels a pending
  reply approval for that item, since the press is the newer decision.
- Button approvals use the sha in the button's value: the commit that held message showed. An old
  button for an earlier head is refused (`sha_mismatch`) and the bot says which head is current.

**Priority words.** The spec's words are applied only where they clearly mean priority, so
"fix the low battery warning" stays P3 with its title intact: `urgent`, `someday` and `P1`–`P5`
anywhere; `highest`/`high`/`medium`/`low`/`lowest` when followed by `priority`/`prio`
("high priority", "low-priority"), as a leading "low:" or a trailing ", low". The first marker
wins; all markers leave the title.

**Control message.** Posted once to `#loop-digest`, its ts stored in the control row. Every start
updates it and pins it again (`already_pinned` is fine), so a pin that failed for want of
`pins:write` succeeds once the scope is added. A failed pin is a logged warning; everything else
works. If the message was deleted (`message_not_found`), a new one is posted. `pins.list` is not
needed and not implemented. − and + clamp to 0–6.

**Digest.** Boundaries are 00/04/08/12/16/20 on New York's wall clock (`time/tzdata` is embedded so a
bare box knows the zone). The first run only records the current boundary (no backfill of history).
Each digest covers `(last, boundary]`; after downtime one digest covers the whole gap. Sections:
merged, held (land → held), needs you (any move into needs_you), failing (`usage_limit` and
`failed`), new; each item once per section. Nothing in any section → no message, but the window
moves on. A post that fails 3 ticks in a row is skipped and logged rather than retried forever.
The interval is 4 hours (`Config.DigestHours`, any divisor of 24); there is no flag for it yet.
