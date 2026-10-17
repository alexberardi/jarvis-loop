# Unservable items: loopd flags an item no runner can claim (2026-10-17)

Item #92. #61 and #63 (resources `macos`) sat in build for hours: the only runner offers
`emulator, gpu, voice`, the claim only hands out items whose resources are a subset of the claiming
runner's capabilities, and nothing said so until the orchestrator's 2h idle trigger, an LLM turn.

## Decision

loopd checks it itself, deterministically (`pipeline.Unservable`, `store.checkUnservable`):

- An item in a work stage with resources is **unservable** when no runner seen within
  `pipeline.RunnerSeenWindow` (24h; `runners.last_seen`, set by claims and heartbeats) offers all of
  them. The missing ones are named (all of them when each is offered somewhere but none has all).
- Checked on every claim (a runner's capabilities may change), every advance (triage sets the
  resources) and once a minute in `Sweep` (a runner may go quiet).
- Flagging keeps the stage (option (a) of the triage notes, not a move to needs_you): a row in
  `unservable`, an `unservable` event (`data.missing`) once per missing set, which the Slack bot
  posts in the item's thread mentioning the user, and a "no runner offers macos" label on the board
  card (and as the root of the wait chains of items depending on it). The orchestrator snapshot
  carries it as `unservable`.
- It clears itself: when a runner offering the resources claims, the row goes with a `servable`
  event and that runner gets the item in the same claim. An item that leaves the work stages loses
  the row silently.
- The orchestrator's idle trigger skips flagged items (loopd told the user already). Its sweep turn
  still sees them and may still file its process ticket; that backstop is unchanged.

Why not needs_you: the user would have to move the item back by hand once a runner exists, and the
item is not asking a question; the flag is loud enough (a mention) and needs no follow-up.
