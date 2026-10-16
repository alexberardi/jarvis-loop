# Phase 3: Slack

Goal (DESIGN.md, Rollout 3): the user runs the pipeline from Slack. Item threads, `/loop`,
questions and answers, the hold buttons and reply approvals, the digest, and the active-agents
control. Still no agents doing work (phase 4); stages are still moved with the `loop` CLI.
**Proves:** the pipeline runs from a phone.

## Facts (do not guess these)

- Workspace `jarvisautomationdev`, bot user `jarvisloop` (bot id `B0CA6SRV664`), Socket Mode on.
- Channels: `#loop` = `C0C9G6Y87HA` (item threads), `#loop-digest` = `C0C969AA2Q3`, `#plan` =
  `C0C8Y132YRM` (plan threads, phase 7; the bot is a member).
- The approver, and the only user whose answers, approvals and control changes count:
  `U0C9AFPAMD4`.
- Scopes: app_mentions:read, channels:history, channels:read, chat:write, commands, files:write,
  users:read, pins:write. The ⏳/✅ reactions on `#plan` messages (phase 7) need `reactions:write`,
  which the app may not have yet: without it they fail and are logged, nothing else. Events: `app_mention`, `message.channels`. Interactivity on. Slash
  command `/loop`.
- Tokens on the dev box: `~/.config/jarvis-harness/slack.env` (`SLACK_BOT_TOKEN` xoxb,
  `SLACK_APP_TOKEN` xapp). loopd reads them from its environment (`--slack-env FILE` loads that file;
  refuse it unless 0600). **Never print, log or commit a token.**

## Shape

`internal/slack`: a Socket Mode client and a Web API client, both small and ours.

- **Web API:** plain `net/http` + JSON for `chat.postMessage`, `chat.update`, `chat.postEphemeral`,
  `pins.add`, `pins.list` (only what is used). Handle `ok:false` errors and HTTP 429 with
  `Retry-After`. Base URL injectable for tests.
- **Socket Mode:** `apps.connections.open` with the app token, then a WebSocket
  (`github.com/coder/websocket`, the one new dependency; record why in a decision file). Ack every
  envelope (`{"envelope_id": …}`) at once, before handling. Reconnect on `disconnect` envelopes and
  dropped connections with backoff. Dedupe on `envelope_id` / the event's `event_id` (Slack retries).
- loopd starts Slack only when `--slack-env` is given; without it everything works as in phase 2.
- **Event fan-out:** the store emits each new event to subscribers after commit; the Slack layer
  turns events into messages. A failed Slack call never fails or rolls back a pipeline action. It
  is retried (bounded) and logged without tokens.

## Threads

- **Item created** → a parent message in `#loop`: `#<id> <title>`, priority, the display stage,
  the source, and a **priority dropdown** (static_select P1–P5, the current one selected). Store
  `slack_channel` and `slack_ts` on the item (migration 0003).
- **Every event** → a short thread reply (one or two lines: stage change, outcome, PR link, CI
  result, hold reason, who approved). The parent's stage line is kept current with `chat.update`.
- **needs_you** → a thread reply that @mentions `<@U0C9AFPAMD4>` with the question.
- **held** → a thread reply with the PR link, the hold reason (the matched rules), the head commit
  (short sha) and **Approve merge** / **Keep holding** buttons. It @mentions the user.
- Size discipline: every message ≤ 2,000 characters; long text is cut with "… (full text on loopd)"
  and a link to `GET /items/{id}` on loopd's address (LAN). Never post evidence contents, logs,
  tokens, or host names.

## The user's actions

Only `U0C9AFPAMD4` counts. Anyone else: an ephemeral "only the approver can do that", nothing
changes, and a `refused` event records who tried.

- **Answer:** a message in an item's thread (`thread_ts` = the item's `slack_ts`) while the item is
  `needs_you` → `answer` with the text.
- **Approve / Keep holding buttons** → the same as below, with the button's own words.
- **Reply approval** while `held`: a thread reply is classified by a small, deterministic,
  table-tested classifier (no model):
  - *approve* when it reads as consent and is not a question: contains an approval word or phrase
    (`approve`, `approved`, `lgtm`, `ship it`, `go ahead`, `merge it`, `fine`, `ok`, `okay`, `yes`,
    `yep`, `yea`, `sure`, `do it`) and no `?`, and no negation nearby (`not`, `don't`, `no`,
    `wait`, `hold`, `stop`).
  - otherwise *not an approval*: the item stays held and the bot replies "Not taken as an
    approval; the item stays held. Reply 'approve' or use the button."
  - On *approve*: post "Approved by you: '<their words>' for `<short sha>`. Merging in 2 minutes."
    with a **Cancel** button. After 2 minutes (injected clock / timer, testable), call `approve`
    with that head sha and the words. Cancel within the window → nothing, and an event. If the
    head changed in between, the approval is refused by loopd as usual and the bot says so.
  - The button approval skips the 2-minute wait (the press is explicit).
- **Priority dropdown** → `PATCH /items/{id}` priority.

## `/loop` and mentions

| Command | Does | Who |
| --- | --- | --- |
| `/loop add <text>` | add an item; priority from words (`highest`/`urgent` P1, `high` P2, `medium` P3, `low` P4, `lowest`/`someday` P5; default P3), title = the text minus those words | the approver |
| `@jarvis-loop <text>` in any channel the bot is in | the same as `/loop add` | the approver |
| `/loop status` | the board, grouped by display stage (ephemeral) | anyone in the workspace |
| `/loop show <id>` | one item: stage, priority, risk + reason, PR, last events (ephemeral) | anyone |
| `/loop priority <id> P2` | change priority | the approver |
| `/loop agents <0-6>` | set active agents | the approver |
| `/loop stop`, `/loop resume` | stop or resume | the approver |
| `/loop usage <N>%` | record a `/usage` reading (an event; used by the digest later) | the approver |
| anything else | a short help text (ephemeral) | anyone |

Slash commands must be acknowledged within 3 seconds: ack with an ephemeral "working…" if a call
might be slow, then answer via the command's `response_url`.

## Active-agents control

A single message in `#loop-digest`: "Active agents: N of 6 (stopped)" with a static_select 0–6 and
− / + buttons. Created on first start and **pinned** (`pins.add`); its `ts` is stored (control row)
so restarts update the same message rather than posting a new one. Every control change (from any
source: Slack, CLI, page, usage_limit) updates it with `chat.update`. Only the approver may change
it.

## Digest

Every 4 hours (configurable; aligned to 00/04/08/12/16/20 America/New_York), a message in
`#loop-digest`: counts and one line per item for **merged**, **held**, **needs you**, **failing**
(usage_limit/failed events), **new**, since the previous digest; the active-agents number; the last
`/loop usage` reading if any. Nothing to report → no message. The last digest time is stored, so a
restart does not double-post.

## Tests (first)

- Classifier: a table of replies → approve / not (incl. "yea, removing that old test is fine",
  "looks fine?", "don't merge yet", "ok wait", "LGTM", "fine by me", "no").
- Priority words parser: a table.
- Socket Mode: against a fake Slack (httptest serving `apps.connections.open` and a WebSocket):
  envelopes acked before handling, duplicate envelope ignored, reconnect on `disconnect`.
- Web API: `ok:false` errors, 429 with Retry-After honoured (fake clock).
- End to end with fake Slack + the real store: item created → parent posted with the dropdown;
  events → thread replies and parent updated; needs_you → mention; answer by thread reply from the
  approver advances it, from another user is refused; held → buttons; reply approval → echo with
  Cancel → 2 minutes later approve called with the head sha; Cancel prevents it; button approve
  immediate; head moved meanwhile → refused and reported; control message created once, pinned,
  updated on change; `/loop` commands incl. permission refusals; digest window and no double-post.
- No real Slack in tests. No sleeps.

## Real run (dev box)

Start loopd with `--slack-env ~/.config/jarvis-harness/slack.env` against a temp data dir; check
the control message appears pinned in `#loop-digest`; add a test item with the CLI and walk it to
`needs_you` and to `held` so its thread shows the question and the buttons. Leave that item in
`held` and the item URL/thread for the user to try the buttons and a reply approval; report what to
try. Post nothing outside `#loop` and `#loop-digest`. Stop loopd afterwards unless told otherwise.
