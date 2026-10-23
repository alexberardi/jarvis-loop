# A waiting stage for items blocked on other work (#501)

2026-10-22. The user: "add a waiting column for situations where a ticket needs another ticket(s)
to finish before it can be re-built/reviewed/merged". Until now a blocked item sat in needs-you or
held, or was re-queued and failed again (#472 and #483 kept failing the macOS verify until
jarvis-server PR #444 merged; #455 waits on #472). `loop link --after` already stopped a claim,
but the item stayed in its own column and nothing brought a needs-you item back.

## Decision

- **`waiting` is a real stage**, not a derived column: parking is explicit, it can take an item out
  of needs-you, and leaving it is an event with a note. It is no work stage: nothing claims it.
  It reuses `ReturnStage` (as needs-you does) for the stage to go back to: build, review or land.
- **What it waits on**: items are its `depends_on` (a wait links them, so a missing or closed item
  and a cycle are refused exactly as `link` refuses them); PRs that are not loop items are
  `wait_prs` (`owner/name#444`, `#444` for the item's repo), each with whether it merged.
  An item is done once merged (qa or done, as for dependencies); a PR once GitHub says MERGED.
- **Who parks**: the user (`loop wait <id> --on 472,483 --pr owner/name#444 [--reason]`,
  `/loop wait <id> on #472 pr owner/name#444 because …`, or a needs-you thread reply starting
  "wait on …"), the orchestrator (`wait` action), and build and review agents (`wait` outcome).
  Only from build, review, land or a needs-you question from one; triage already waits for its
  dependencies (link it), and a held item waits for the user's approval. An item a run holds is
  parked by its own agent, not from outside.
- **Release**: loopd checks the waiting items after every transition (a merge, an unlink, a PR's
  merge) and once a minute; one with nothing left goes back to its stage, recorded by loopd as
  `released` with a note ("#472 merged, owner/name#444 merged: back to review"), posted in its
  thread. `loop unwait` ends a wait by hand.
- **PRs are watched by the runners**: loopd holds no GitHub login. Every poll a runner asks for the
  waiting items and checks, with its own `gh`, the PRs of the repos it has; it reports MERGED or
  CLOSED (`POST /items/{id}/wait-pr`, no lease, like the held watch).
- **Escalation**: a dependency dropped while it waits, or a PR closed without merging, sends the
  item to needs-you with the reason; the answer sends it back to the stage it was waiting to
  return to. A wait that would make a cycle is refused (the user's, the orchestrator's); an agent's
  refused wait sends its item to needs-you with the refusal, since nobody else would see it.
- **Shown**: the board's Waiting column (card: "waiting on #472, owner/name#444, then back to
  review"), `loop status`'s WAITING list, `/loop status`, `loop show`, the orchestrator's snapshot,
  the flow panel (as "dependencies"); the standup lists waits under a day under Waiting and longer
  ones under "Waiting over a day".
- The agent's `wait` is applied by the store before the outcome table (`pipeline/approve.go`, on the
  hold list), so that table is unchanged.
