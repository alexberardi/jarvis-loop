# #plan in Slack: reactions, message ts, and the approval window

2026-10-16 · phase 7, piece 4 (`docs/phases/07-planning.md`, "Slack").

- **The bot remembers which Slack messages each turn carries** (`plan_turns.slack_ts`, migration
  `0009_plan_slack.sql`), rather than keeping them in memory or reading the thread back from Slack:
  ⏳/✅ must land on the right messages after a restart, and a requeued turn's messages (lease
  expired, loop stopped) move with it. It also keeps its header message's ts
  (`plans.slack_header`) so it edits that message instead of posting stage changes as new ones.
- **Reactions are a nicety.** They need the `reactions:write` scope, which the Slack app may not
  have (03-slack.md's scope list predates it). A failed `reactions.add`/`reactions.remove` is
  logged as a warning and nothing else, like a failed pin: the reply in the thread is what matters.
  Add the scope in the app's settings and reinstall to see them.
- **An Approve press waits 2 minutes, with Cancel**, as a reply approval of a merge does, rather than
  applying at once as the Approve merge button does: the spec asks for "Echo, Cancel and the
  2-minute grace as for merges", and approving a blueprint files items in one go. The approval is
  bound to the sha256 the button was posted for, so a press on an old reply is refused with the
  newer version.
- **Replies in a plan thread are always conversation**: no reply word approves a plan (only the
  button), so "ok, but add the freezer" goes to the agent.
- **Anyone else in #plan is ignored silently** (logged only), as the spec's scenario 6 says, unlike
  `#loop` where a non-approver's reply gets an ephemeral "only the approver" and a `refused` line:
  a plan thread is a conversation, and the bot answering others there would read as the agent's.
  Their button presses are still refused as in `#loop`.
