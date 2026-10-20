# Held items go back to build: a conflict, or the user's note

2026-10-16 · item #33. **Seen:** a held PR that turned `CONFLICTING` sat held until the user
approved it, only for land to find the conflict then; and a reply in a held thread such as
"rebuild" or "rebase and fix X" was dropped with "Not taken as an approval". What was chosen, each
covered by a test or a scenario:

- **One transition, Held → Build** (`pipeline.ApplyRebuild`, event kind `rebuild`, data `notes`).
  Only from held; notes are required. The PR, branch, risk and hold reason stay; an approval is
  voided (the build makes a new head, which the user approves again). It is **not a bounce**, as a
  land conflict is not one today. `POST /items/{id}/rebuild` takes a user or a runner token and no
  lease: the only item it moves is a held one, and only back to build, which cannot merge anything.
- **The conflict (runner).** `Runner.CheckHeld`, every land poll (`LandPollEvery`, 30 s) on a
  runner with the land stage and a GitHub client, next to the land lane rather than in an agent
  slot: it lists the held items (`GET /items?stage=held`), skips other runners' repos, reads each
  PR (`LandView`) and sends an open `CONFLICTING` one to build with "PR #n conflicts with main while
  it was held. Merge origin/main into the branch, …". A `wrong_stage` answer (the user approved
  first, or another runner got there) is ignored. **First cut:** every conflict goes to build;
  land's own merge of a docs-only conflict (`landConflict`, which pushes and goes back to review)
  is not reused here, because it needs a land lease and the mirror lock for an item nobody claimed.
  A docs-only conflict therefore costs a build run instead of none. *Superseded 2026-10-18:* the
  runner now merges what git can (clean or docs only) and sends the new head to review
  ([approval-carry](2026-10-18-approval-carry.md)).
- **The note (Slack).** In a held thread, the approver's reply is a note when `IsHeldNote` says
  so. It is checked **before** `ClassifyReply`, so a change request wins over consent words in it:
  "ok, rebuild", "yes, but rebase first" and "merge it after adding a test" go back to build, not
  to a merge; "lgtm", "ship it", "merge it" and "merge it please" are not notes and still start
  an approval. The match is positive: after mentions and leading "please", "ok", "yes", "sure",
  "but" or "actually", the reply opens with a change verb (rebuild, rebase,
  fix, rename, change, add, remove, update, use, revert, resolve, …), or is "merge main …", or
  "merge …" with a change verb later ("merge it after adding a test"). A question mark, a
  question opening or a negation or "wait"/"hold"/"stop" word anywhere is never a note. Strict
  and model-free like the approval check: "looks good", "nice work", "let me check", "later",
  "merge it please", "looks fine?", "don't merge yet" and "fix it but don't touch X" stay held,
  with the hint now saying a note sends it back.
  A note cancels a pending reply approval. Someone else's reply is refused as before. The thread
  line is "Sent back to build by you: '…'." (or "Back to build: …" for the runner).
- **The builder sees it.** A build run of an item with a PR writes the `rebuild` notes since the
  last `pr_opened` to `.loop/review/held.md` (who sent it back, when, the notes), and the brief
  says "Sent back from held; PR #n is open on this branch. Do what `.loop/review/held.md` asks".

**Since 2026-10-17 (item #131):** a needs_you item can go back to build the same way (a reply
that opens with rebuild, rebase or fix, or the Rebuild… button on needs_you and held messages);
see `2026-10-17-needs-you-rebuild.md`. The context file's label and heading no longer say "held".

**Not done:** the held message's buttons are not updated when the item leaves held this way (a
later press gets the store's "item is build, not held"), as with a reply approval today.
