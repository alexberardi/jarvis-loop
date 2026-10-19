# Phase 8: the orchestrator

Goal: one model owns keeping the board moving, the way a scrum master would. It watches every
item, unsticks what it is allowed to, asks the user only for real decisions (each with a
recommendation and one tap to accept it), and turns repeated trouble into priority tickets
against the loop itself. **Proves:** a day of normal work needs the user only for approvals,
product calls and QA, never to explain to an agent how to get unstuck, and never in a separate
chat with Claude.

Status: **designed** (2026-10-16; the user's answers are in "Decisions" at the end). Built: piece 1 (the
orchestrator token and its actions, [decision](../decisions/2026-10-16-orchestrator-token.md)), piece
3 (the runner's orchestrate turns, [decision](../decisions/2026-10-17-orchestrate-turns.md)), piece 4
(the persona and the recorded-situation scenarios, [decision](../decisions/2026-10-17-orchestrator-persona.md)),
piece 2 (the `@orchestrator` Slack app, [decision](../decisions/2026-10-17-orchestrator-slack.md)).

## Why (2026-10-16, one day of real use)

Every stuck item that day needed the user to open a chat and ask Claude what was going on. What
they needed, by kind:

| Stuck because | Count | What moved it | What should have |
|---|---|---|---|
| Runner or environment error (git lock, the model off, a killed run) | ~15 | a retry | retry with backoff, ask after the third |
| The agent re-asked an answered question | ~6 | nothing (bug, fixed in #39) | — |
| The agent can't run the harness or the emulator | ~6 | "skip it, list it under not verified" | a standing answer (until #67 gives agents the harness) |
| A change to a trusted file (deny_write) | 2 | no way forward | the user's sha-bound OK (#70) |
| Review bounce limit | 2 | "go on with the reviewer's notes" | the same, by the orchestrator, once |
| Same-head red CI | 2 | reading the log: flaky or real | re-run once; then send back with the log |
| Waiting on a dependency | 18 | nothing (but invisible) | the board says what the chain waits on |
| Loop config mistakes (1 slot, no capabilities) | 2 | a person noticing | "items ready but nothing claims them" is a pattern it reports |

Most of these are not decisions. They are the same few moves, made by whoever happens to be
watching. The orchestrator is that watcher.

## What it is

- **A runner turn kind, `orchestrate`**, like the plan turns (phase 7): a headless `claude -p`
  that resumes one session, with no tools that write anything but its result. It holds no
  credentials. The runner gives it a snapshot and executes the actions in its result through
  loopd, which checks each one against the orchestrator's authority.
- **Triggered by loopd**, not by a timer that runs every turn: an item entering needs-you, a run
  failing, a lease expiring, an item idle in one stage longer than its stage allows, a merge to
  the loop's own repo, a user message to it. Plus a **sweep every 15 minutes** while anything is
  open (a no-op turn costs one short call). Events that arrive during a turn join the next one.
  One orchestrator turn at a time; it shares the active-agents limit but always gets the next
  free slot.
- **Memory**: the resumed session (rotated daily so its context stays small) and a notebook file
  it keeps (`notebook.md`, on loopd, shown on the board): what it is watching, what it tried,
  patterns it suspects.

### What each turn sees

- The board as data: every open item (stage, priority, deps and what the chain waits on, bounce
  count, age in stage, question), live leases with their activity (#68), runner health
  (slots, capabilities, last claim), the active-agents number, usage so far today.
- For each item that triggered the turn: its events, its question, its evidence list, and for a
  runner error the end of the runner's log for that run.
- Its notebook, its open escalations, and the user's replies since its last turn.

### What it may do (loopd enforces it)

A new token kind, `orchestrator`, with these actions only, each recorded as an event with its
reason (shown in the item's Slack thread as "Orchestrator: …" and on the board):

| Action | Limits |
|---|---|
| **answer** an item's question | only the kinds in "Answers it may give"; at most 2 per item per head sha; never the same answer twice in a row |
| **retry** a run that failed on the runner or the environment | backoff 2, 10, 30 min; the third failure on the same head is escalated |
| **priority** of an item | P1–P5 (the user's choice), with the reason in the event |
| **add** an item (process tickets, follow-ups) | at most 5 a day; deduplicated against open items by a failure signature |
| **link** a dependency (`--after`) | only to an existing open item |
| **escalate** to the user | see below; at most one ping per 10 minutes outside approvals |
| **agents** (the active-agents number) | down only (to protect usage or stop a systemic failure), never above the user's number; always with a ping |
| **note** to its notebook | — |

**Never:** approve a merge, OK a trusted-file change (#70), drop or close an item, change the
runner's config, the hold rules or a gate, deploy, touch prod, edit code. It may *recommend* any
of these to the user.

### Answers it may give

1. A question the item's history already answers (the newest answer wins).
2. "The harness / emulator / model can't run in your sandbox": the standing answer (list it under
   not verified; the runner's verify runs it), until #67 lands.
3. A runner or environment error: retry, after checking the error's signature isn't already
   failing every run (then it is a process issue, below, not a retry).
4. Review bounce limit with concrete must-fix notes: "go on with the reviewer's notes", once per
   item.
5. Same-head red CI: re-run the failed jobs once; if it fails again, send it back to build with the
   log ("fix the code, not the expectation").
6. Questions about how the loop works (where evidence goes, what a stage does).

Anything else is the user's: product or design choices, scope, a change on a hold list, a
security boundary, approvals, trusted-file OKs, dropping, prod, a third attempt on one item, and
anything it isn't sure about.

### Asking the user

In the item's thread, mentioning the user, one message:

> **#46 mobile-shot: the newest inbox item's detail** needs you: the agent can't take the
> screenshots the item asks for (no emulator in its sandbox).
> **Recommend:** open the PR without them, with the run listed as not verified; the runner's
> verify takes them. *Because* the code and its tests pass, and #67 will let agents run it.
> [Accept] [Other…]

- **Accept** records the recommendation as the user's answer (approver only, like approvals).
  **Other…** or any reply in the thread is the answer instead. Approvals keep their own buttons.
- Pings are batched: several items that need the user within a few minutes go in one message.
  No quiet hours (the user's choice).

### Process issues: tickets against the loop

After each turn it looks across items, not just at one:

- the same failure signature on two or more items (normalised: paths, shas and numbers removed);
- items ready for an hour with free slots and no claim (capabilities, limits, slots);
- an item asking the same kind of question twice;
- a scenario or test failing then passing on rerun (flaky);
- a stage taking far longer than its usual time.

For each it files (or adds evidence to) a **P1 item in jarvis-loop** with the signature, the
items affected and the log lines, and writes it in its notebook. If the issue makes every run
fail, it lowers active agents to stop the waste and pings the user. Its tickets go through the
loop like any other item, held where the hold rules say.

### Talking to it

- A daily **standup** in `#loop-digest` at 09:00: merged yesterday, in flight, waiting and on
  what, what it did, what needs the user (replacing the 4-hourly digest's item list).
- `@jarvis-loop` in `#loop` with a question ("what's stuck?", "why is #35 waiting?", "move the
  autonomy items up") starts an orchestrator turn that answers in the thread, within its
  authority; anything beyond it comes back as a recommendation.
- The board gets an **Orchestrator** panel: last turn, recent actions with their reasons, open
  escalations, its tickets, and its notebook. Every card that waits says on what ("waiting on
  #35 → needs you").

### Its own persona in Slack

The orchestrator is a **separate Slack app, `@orchestrator`** (its own bot user, name and icon; a
manifest in `docs/slack/orchestrator-manifest.yml`, created once by the user), so the user can
mention it in any thread or DM it directly. loopd holds its tokens next to the loop bot's and
keeps one socket per app.

- **It acts for the user when the user tells it to.** "drop #61 and #63", "move the autonomy items
  up", "answer #46: open the PR without the PNGs", "pause the loop" — run as the user (the actor
  is the user, "via orchestrator"), so they may go beyond its own authority, including drops.
  **Approvals and trusted-file OKs stay on their sha-bound buttons**: asked to approve, it posts
  the item's approval message with the button instead.
- Only the approver's messages can make it act; anyone else gets an answer, never an action.
- When it acts on its own, it stays within "What it may do".
- Claude chats stop handling loop items once it runs: stuck items are its job.

## Cost

A turn is small (the snapshot, a few items, its notebook) and resumes a cached session. Sweeps
with nothing new are skipped by loopd before any model call (it only starts a turn when
something changed or an escalation is due). Cap: 150 turns a day, configurable; the daily
standup reports its own spend.

## Pieces (filed as loop items, in order, held for the user)

0. Prerequisites in flight: #67 (agents use the harness), #70 (trusted-file OK).
1. **loopd: the orchestrator token kind and its actions** (answer, retry, priority, add, link,
   escalate, agents-down, note), each limit enforced and tested; events with reasons.
2. **Slack: the `@orchestrator` app** (manifest, second socket, mentions and DMs), recommendations
   with Accept, batching, "Orchestrator: …" lines, and acting for the user on the approver's
   say-so.
3. **Runner: orchestrate turns**: triggers, snapshot, result contract, notebook, daily rotation.
4. **The orchestrator persona** (its skill: the authority table, the answer kinds, how to write a
   recommendation, process-issue patterns), with scenario tests on recorded situations from
   2026-10-16. As built: `skills/orchestrator/SKILL.md` is the first part of each day's first
   prompt (`runner.OrchStatic`, the result contract after it). The scenarios
   `59-orchestrator-{lock-error,reasked-question,harness,bounce-limit,red-ci,unclaimed}` play the
   recorded situations against the fake claude's `persona` play, the skill as rules on the
   snapshot: they test how loopd carries out the persona's actions, not the model's judgment.
5. **Board: the Orchestrator panel and "waiting on" for every card.** As built: each dependency
   not merged (qa or done) is followed to its root (`pipeline.WaitChains`; past the first link,
   the first unmerged dependency of each item), "waiting on #12 → #35 → needs you". The panel
   (`Store.OrchestratorBoard`, hidden until the orchestrator acts, open while an escalation is)
   lists its last 15 actions with reasons (its refusals and what the user did through it too),
   open escalations (about an item: until it changes stage or is done; about none: for a day), its
   tickets (open first) and its notebook through the board's markdown allow-list. "Last turn" is
   its newest action or note until piece 3 records turns.
6. **Standup and `@jarvis-loop` questions.** As built (docs/decisions/2026-10-18-standup.md): a
   message trigger carries its channel, thread and whether the approver wrote it; the turn
   answers with a `reply` action in that thread (and `ask_approval` when asked to approve). The
   09:00 standup is composed by loopd from the store (no model call) and posted by `@orchestrator`
   in `#loop-digest`: the 24 hours to 09:00, its spend from each turn's recorded cost and tokens.
   The digest counts the items instead of listing them. Scenarios `68-orchestrator-answers-in-thread`
   and `68-standup`.

## Harness

loop-sandbox scenarios, test-first: a runner error is retried then escalated on the third try; an
answered question is answered from history; a bounce limit is sent back once, then escalated; a
product question is escalated with a recommendation and Accept records it as the user's answer;
two items with the same error signature produce one P1 ticket; the orchestrator's own approve, drop
or OK is refused by loopd; the approver's "drop #61" to `@orchestrator` drops it as the user;
someone else's is refused; "approve #28" posts the approval button instead of approving.

## Decisions (user, 2026-10-16)

1. **Model:** the same as the builders.
2. **Quiet hours:** none; pings are batched.
3. **Priorities:** it may change any priority, P1 included, on its own.
4. **Drops:** never on its own; only when the user tells it to.
5. **Who handles stuck items:** the orchestrator only, as its own Slack persona the user can ping
   directly to take action. Claude chats stop doing it.
