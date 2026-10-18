# Jarvis Loop — agent pipeline design

2026-10-14 · Living version, with the two drawings: [Jarvis Loop — agent pipeline design](https://claude.ai/code/artifact/6763c138-bca4-4056-adc0-48a53771919c) (comment there; this file follows it).

Jarvis Loop turns the hand-run agent work of 2026-10-13/14 (jarvis-server PRs #127–#135, jarvis-node-mobile #103) into a pipeline: work enters, is reproduced, built, reviewed by a second agent and landed under the merge rule, with the user in the loop only for questions and held merges, through Slack. A small server, `loopd`, holds the state on the prod box; runners on the machines with the GPU, emulator or Mac do the work.

## At a glance

*(Drawing in the living doc: you ↔ Slack ↔ loopd on the prod server (pipeline state, stage rules, evidence store); runners on the dev box and later the Mac claim work from loopd and push to GitHub.)*

`loopd` never touches GitHub: runners push and merge with their own credentials, so the server holding the state cannot change code.

## The pipeline

*(Drawing in the living doc: intake → triage → ready → build → review → land → done, with the two human gates on the side: needs-you, fed by triage questions and by review after 2 bounces; held, fed by land for hold-list PRs. Since 2026-10-15 an opt-in third one after land: qa, for merged items only the user can check.)*

Work moves one stage at a time and cannot skip one; only the two gates wait on the user, both through Slack.

| Stage | Owner | Leaves when | Limit |
| --- | --- | --- | --- |
| Intake | the user, harness findings, nightly runs | it has an ask, a source and a priority; it then moves on by itself, no approval needed | none |
| Triage | an agent | reproduced on `main` with evidence, checked against open and recently merged work, sized, risk and resources set; or closed as not reproducible, with the evidence | 1 at a time |
| Ready | the queue | a runner with the right resources claims it, highest priority first, then oldest | per resource (Runners) |
| Build | a worker agent in its own worktree | a test-first fix, harness before/after attached, `scripts/check` green, PR open | the active-agents number |
| Review | a different agent from the builder | it re-ran the evidence and reviewed the code: approve, or bounce to build with notes (after 2 bounces → needs-you) | 2 at a time |
| Land | a script | merged with `main` green, or held; red CI goes back to build as a counted bounce, and a rebuild that leaves the red head unchanged goes to needs-you ([decision](decisions/2026-10-15-ci-red-unchanged-head.md)) | 1 at a time |
| Needs-you | the user, in the item's thread | answered; it returns to the stage that asked | none |
| Held | the user, button or reply | approved (merge, then done), kept holding, or sent back to build: by the user's reply that is not an approval (a note for the builder), or on its own when the held PR turns conflicting ([decision](decisions/2026-10-16-held-back-to-build.md)) | none |
| QA | the user, after merge (opt-in) | **Verified** → done, or **Reopen** with words → done, and a new P2 item with those words. Only items marked needs_qa (by triage, review, `/loop qa <id>` before land, or the repo's `qa: true`) come here instead of done; nothing claims it ([decision](decisions/2026-10-15-qa.md)). For the loop's own repo (`qa_after_deploy: true`) the check is asked for once a runner built from a commit containing the merge reports `deployed` ([decision](decisions/2026-10-16-qa-after-deploy.md)) | none |

## Priority

Every item has a priority, and the ready queue is worked strictly by it: all P1s before any P2, oldest first within a tier. It is required, so an item never waits without one.

| Tier | Say in Slack | Meaning |
| --- | --- | --- |
| P1 | "highest", "urgent" | drop everything: a broken `main`, a regression in the house |
| P2 | "high" | next up |
| P3 | "medium", or nothing said | the default |
| P4 | "low" | when nothing more important waits |
| P5 | "lowest", "someday" | only on an otherwise idle queue |

Set in words when adding ("this is a high priority feature") or with the dropdown on the item's first message in `#loop`, which also changes it later. Work the loop files itself (a finding, a nightly failure) starts at P3, a red `main` at P1, an accepted scout suggestion at P4.

## Planning, before the loop

New features start in `#plan`, not in the loop: two planning stages turn an idea into loop-sized items that each carry a blueprint, so build agents implement a plan instead of guessing one (decided 2026-10-14).

| Stage | Who | Produces | Leaves when |
| --- | --- | --- | --- |
| **P1 Product** | a product-persona agent, in conversation with the user in the feature's `#plan` thread | a **feature brief**: who it is for, the problem, what they would actually want, success criteria, out of scope | the user approves the brief (button or reply, as for held merges) |
| **P2 Engineering** | an engineering-persona agent that reads the brief and the real code | a **feasibility note and blueprint**: approach, affected modules, interfaces, test plan, a harness verification plan (which scenarios and shots prove it), risks, and a split into loop-sized pieces | the user approves the blueprint; not feasible or much bigger than expected → back to P1 in the thread |
| **Hand-off** | `loopd` | one loop item per piece, at the feature's priority, each with its section of the blueprint and **dependencies** (piece 2 waits until piece 1 is merged) | the items are in intake |

**Where briefs and blueprints live:** as pages (like the living design doc) while they are written, so the user can read and comment from a phone; a final copy is committed to the target repo (`docs/plans/<feature>.md`) when the first item starts, so agents read it in their worktree. Every item, thread and PR links back to its plan.

**What the loop gains:** item dependencies; a plan link on items; and conversational stages, where the runner starts a short agent per message in a `#plan` thread with the thread as context, instead of one agent per stage. Triage checks that the blueprint still matches the code; review and the harness gate check the work against the blueprint's own verification plan.

## loopd

One Go binary with SQLite and a small embedded web page, in this repo. It owns the pipeline's state and enforces its rules: stage order, limits, the review-by-another-agent rule and the bounce limit. Agents and the user only ask it to move things.

**Data model**

| Table | Holds |
| --- | --- |
| items | one unit of work: title, ask, source (user, finding, nightly, scout), repo, stage, area, size, risk (`hold` or not), resources it needs, priority, bounce count, PR, branch |
| events | every change to an item, append-only: stage moves, questions and answers, approvals (who, words, commit), CI results. The item's history and its Slack thread are both built from it |
| evidence | files attached to an item: screenshots, scenario logs, `llm.jsonl` slices, diffs. Stored on the server's disk, served behind a LAN link |
| runners | each machine that does work: name, capabilities (`gpu`, `voice`, `emulator`, `macos`), last heartbeat |
| leases | which runner and agent hold which item at which stage, with an expiry kept alive by heartbeats. An expired lease returns the item to its queue |
| control | the active-agents number (0–6), stop, the per-run time cap |

**API** (JSON over HTTP, a bearer token per runner and one for the user; a `board` token reads the web board only and is refused by every API route; an `orchestrator` token reads and acts only through `/orchestrator/…` within its limits, [decision](decisions/2026-10-16-orchestrator-token.md))

| Call | Does |
| --- | --- |
| `POST /items` | add an item (from Slack, the CLI, a finding) |
| `GET /items`, `GET /items/{id}` | the board and one item with its history |
| `PATCH /items/{id}` | change priority or title |
| `POST /claim` | a runner asks for work matching its capabilities; returns an item, its stage brief and a lease, or nothing |
| `POST /leases/{id}/heartbeat` | keep a lease alive, with an optional `activity` (what the agent is doing, for the board's "Working now"; [decision](decisions/2026-10-16-working-now.md)); the answer says whether to stop |
| `POST /items/{id}/advance` | finish a stage with its result and evidence; the server checks the exit criteria and picks the next stage |
| `POST /items/{id}/evidence` | upload a file |
| `POST /items/{id}/ask`, `/answer` | put an item in needs-you with a question; record the answer |
| `POST /items/{id}/approve`, `/hold`, `/rebuild` | the decision on a held merge; `/rebuild` (the user, or a runner for a held PR that turned conflicting) sends it back to build with notes; *since 2026-10-17* the user's also takes a needs-you item whose question came after triage ([decision](decisions/2026-10-17-needs-you-rebuild.md)) |
| `POST /items/{id}/drop` | the user closes an item they won't land, from any open stage; the runner then closes its PR |
| `POST /items/{id}/edit`, `/link`, `/unlink`, `/resources` | *since 2026-10-18*, the user's corrections to a filed item, each an event with what it replaced: the title and ask before build starts, the items it waits for, the resources after triage ([decision](decisions/2026-10-18-item-corrections.md)) |
| `POST /items/{id}/qa`, `/verified`, `/reopen` | the QA lane: mark an item for the user's check before it lands; the check passed (qa → done) or failed (qa → done, and a new P2 item with the words) |
| `GET`, `PUT /control` | the active-agents number, stop and resume |
| `GET /board`, `/board/items/{id}`, `/board/items/{id}/evidence/{n}` | the swim-lane web board (HTML, read-only), also on the optional LAN listener `--board-addr`, which serves nothing else ([decision](decisions/2026-10-15-board.md)) |

The `loop` CLI wraps the same API: `loop add` (`--json` or `-q` for scripts), `loop status`, `loop show`, `loop answer`, `loop edit`, `loop link`/`unlink`, `loop resources`, `loop rebuild`, `loop agents <n>`; its flags go anywhere on the line.

## Runners

A runner is a small daemon on a machine that can do the work. It polls `loopd` continuously, claims what matches its capabilities, and starts one headless `claude -p` per claimed item, in a fresh worktree, with that stage's brief. The dev box runs the first runner (`gpu`, `voice`, `emulator`); the Mac can join later (`macos`, `ios-sim`).

**Limits**, set in `loopd` so they can change without a redeploy:

| Resource | Limit | Why |
| --- | --- | --- |
| agents in total | the active-agents number (0–6, default 4) | the user's throttle (Usage control) |
| `gpu` (the shared live model) | 2 | the installed engine has one slot; the house comes first |
| `voice` (CPU whisper) | 1 | about 2.4 s of CPU per utterance at 8 threads |
| `emulator` | 1 | a headless emulator plus a release build is heavy |

**The brief every agent gets** (one skill per stage in this repo, sharing a preamble) carries the rules 2026-10-13/14 taught:

- Work only in your own worktree and sandbox name; never touch the user's checkouts, the user's emulator (`emulator-5554`) or processes you did not start.
- Fetch the sherpa libs for every OS before `scripts/check` in a fresh jarvis-server worktree; put Go's temp dir under `~/.cache` when `/tmp` is short.
- Evidence over claims: before/after from the harness, attached to the item; say what was *not* verified.
- Never `gh pr merge --auto` (no required checks in jarvis-server, so it merges at once); landing is the land stage's job, not the builder's.

**As built (phase 6b, 2026-10-15): every active repo.** The runner's config lists each repo with a
`group` (server, loop, mobile, docs, installer, package), `aliases`, `setup` commands (dependencies
installed by the runner before the agent) and `verify` steps. An item with no repo is **routed
first**: a triage run with no checkouts reads `repos.md` (group, aliases, description, recent PR
titles) and returns `routed`; the next triage run has that one repo checked out. After a build, the
runner itself runs the repo's **verify steps** outside the agent's sandbox but inside a bubblewrap
fence (credentials masked, no tokens, no pushing; fail closed), one at a time per machine, from its
own config only: the logs and files they write are the item's evidence and are
listed apart in the PR's Verification footer; a failure sends the build back (counted like a
bounce) with nothing pushed. A change to a file the runner trusts (`deny_write`) is not verified
until the approver OKs that exact commit (Trust button or `loop trust`); verify then runs it (the OK holds for that commit until verify reaches a verdict on it: the PR, or a real regression; *since 2026-10-18* the build claim after the OK runs no agent: the runner verifies the OK'd head with the result the refused build left, #203)
and land holds the PR. Details: `docs/decisions/2026-10-15-all-repos.md`,
`docs/decisions/2026-10-16-trusted-files.md`. While a build or review agent works,
a repo with a `harness` lets it use the harness through the runner too: `loop-harness up | run | down`
asks the runner, through a spool in the agent's own worktree (checked by the run's token and its guard), to run the command in the
same fence, next to a keeper fence that holds the running sandbox until the agent ends. Details:
`docs/decisions/2026-10-16-harness-broker.md`.

**Cleanup** is the runner's, not the agent's: when a lease ends for any reason it removes that item's worktree, takes down its sandbox and stops its emulator. It never touches anything it did not start, so a stray process from another session is reported, not killed.

## Slack

Slack is the user's whole interface: add work, answer questions and approve merges there, and only get pinged for those. The bot is `jarvis-loop` in jarvisautomationdev, on Socket Mode, so `loopd` needs no public URL. Posting, mentions and thread replies were tested end to end on 2026-10-14; buttons and `/loop` arrive with `loopd`.

| The user does | In Slack |
| --- | --- |
| add work | `/loop add <what>`, or mention `@jarvis-loop` anywhere; name the repo with `[mobile] …`, `nest: …` or `… in docs` (aliases, phase 6b), or leave it to triage |
| follow an item | its one thread in `#loop`: every stage change, the PR, CI, the merge |
| answer a question | reply in the item's thread; the bot @mentions the user when it asks |
| decide a held merge | **Approve merge** / **Keep holding** buttons, or a plain reply (Landing) |
| drop an item | **Drop** on a held or needs-you message, or `/loop drop <id> [why]` (`loop drop <id>` on the CLI); its PR is closed, the branch stays |
| check a merged change yourself | `/loop qa <id>` before it lands (or a repo's `qa: true`); then **Verified** / **Reopen** on the "Merged — waiting for your check" message, or `/loop verified <id>`, `/loop reopen <id> <what is wrong>` |
| set priority | words when adding, or the dropdown on the item's first message |
| see the board | `/loop status`; `/loop show <id>` for one item; on the LAN, the web board at `/board` (a "Working now" card per running agent, refreshed every 5 s; columns per stage, read-only, refreshes every 30 s) |
| turn spending up or down | the pinned **Active agents** message, `/loop agents <n>`, `/loop stop` |
| catch up | a digest every 4 hours in `#loop-digest`: merged, held, failing, new, waiting for your check (qa), estimated spend |

**What never goes to Slack** (a third party): full logs, model-call transcripts, sandbox credentials, tokens, and host names from `HOSTS.local.md`. The bot posts summaries, PR links and small evidence (a cropped screenshot, one failing scenario line); everything else is a link to `loopd` that works only on the LAN, or through the tunnel later.

## Usage control

Claude Code gives a runner no reading of plan usage (`/usage` is interactive only; `claude -p` reports tokens and a cost estimate, not limits; checked against the docs 2026-10-14), so the loop does not guess. The user sets the throttle.

**Active agents:** one number, 0 to 6, is the most agents the loop may run at once; 0 is paused. Turning it down lets running agents finish their current stage; no new claims start above the new number.

| Where | Control |
| --- | --- |
| Slack | a pinned **Active agents** message in `#loop-digest` with a 0–6 dropdown and − / + buttons (Slack has no slider), and `/loop agents <n>` |
| loopd's page (LAN) | a slider |

**Stop:** running agents stop at their next heartbeat (within a minute), their work is saved as a WIP branch, and each item returns to its queue; a Claude build resumes its session on the next claim on the same runner ([decision](decisions/2026-10-18-build-resume.md)). **Resume** clears it.

**Automatic:** when an agent run ends with a usage-limit or rate-limit error, `loopd` sets the active-agents number to 0, posts the error with its reset time in `#loop-digest` with an @mention, and does not raise it again on its own.

**Advisory, not enforced:** every digest shows the loop's estimated spend today (from each run's tokens and `total_cost_usd`) against about 1/7 of a week, learned when the weekly limit is hit and whenever the user posts `/loop usage 42%` from `/usage`. It tells the user when to turn the number down; it never changes it. The loop only sees its own spend, not interactive sessions.

**Prompt caching** (user, 2026-10-14: a previous agent setup burned usage far faster than terminal use). Every agent run for a stage starts from a byte-identical system prompt, tool set and settings; the item's brief is the first user message, never the system prompt; no MCP servers in agent runs; nothing dynamic at the start. Conversational stages (the `#plan` threads) resume one Claude session per thread instead of replaying the thread. Every run records cache-read and cache-write tokens; the digest shows the loop's cache hit rate and flags runs below 50%.

**Per run:** 45 minutes at most; then the item goes to needs-you with what it had so far.

**Scout, when there is room:** if the queue is empty and the number allows another agent, a scout agent looks for work (open STATUS items, harness findings, flaky tests, gaps in scenario coverage) and posts up to three suggestions in `#loop` with **Accept** buttons. Accepted ones enter at P4; nothing starts that the user did not accept.

## Landing

A PR merges when it passes three gates, in every repo (the user's rule, 2026-10-14). Landing is a script that checks them.

| Gate | Passes when | Proof in the PR |
| --- | --- | --- |
| **Agent review** | a different agent from the builder approves, having judged that the tests cover the change *meaningfully* (the new behaviour and its failure cases), not just that tests exist | the review, posted as a PR review |
| **Harness verification** | the item's harness runs pass: the scenarios it touches, before and after screenshots for UI, the desktop or mobile shots where relevant | screenshots and scenario results in the PR description, plus the full logs on `loopd` |
| **CI** | every check on the PR's current head has passed (never `gh pr merge --auto`) | the checks |

The script then:

1. If `main` moved and the PR no longer merges, merges `main` in, pushes, and waits for all three gates again. A conflict outside `docs/` sends the item back to build.
2. Squash-merges, and watches `main`'s CI on the merge commit. Red files a P1 item linked to this one and @mentions the user. Red (still red after one re-run of the failed jobs) also freezes the repo until `main` is green: nothing else in it moves stages, runs already going finish and their results wait, and the P1 goes first ([decision](decisions/2026-10-18-main-red-freeze.md)).

**As built (phase 6, 2026-10-15):** the script is the runner's `land` stage, plain code with no agent: it holds the GitHub login, loopd still holds none. Every pass re-derives the gates from GitHub and git (the PR, review approval for its current head, the changed paths, CI, the harness evidence, the hold list, mergeability); it merges only with `--match-head-commit` (never `--auto` or `--admin`), checks the hold rules itself before merging, resolves conflicts only under `docs/` (git's union merge) and sends any new head back to review. Hold rules are per repo: jarvis-loop's own list holds changes to who may approve or merge, token handling and the landing gates, so the loop never lands a change to itself without the user. Details: `docs/decisions/2026-10-15-land.md`.

**Hold list (it stays):** jarvis-server's `CLAUDE.md` also holds PRs that change auth or access, update signing or the helper, or prod, that weaken a test or CI check, or that are marked "needs review". They still need the three gates, and then the user's approval, by button or in plain words in the item's thread ("yea, removing that old test is fine"):

- Only the user's member ID (`U0C9AFPAMD4`), in that item's own thread, counts.
- The bot decides whether the reply approves; anything unclear is a question, answered in the thread while the item stays held. *Since 2026-10-16:* a reply that is neither an approval nor a question, a "wait" or a "no" ("rebuild", "rebase and fix X") is a note for the builder: the item goes back to build with it, on the same PR. *Since 2026-10-17:* a needs-you reply that opens with rebuild, rebase or fix does the same instead of answering, and needs-you and held messages have a Rebuild… button with a note form ([decision](decisions/2026-10-17-needs-you-rebuild.md)).
- It posts what it understood ("Approved by you: '…', merging #142") with a **Cancel** button, and merges after 2 minutes.
- An approval is for the PR's head commit when given; new commits after it expire it, and the bot asks again.
- The item's history records who, the words, and the commit.

**Screenshots in public PRs:** jarvis-server is public, so PR screenshots come only from sandboxes with the seeded demo data, never from the real install.

**Repos:** the same rule for jarvis-server and jarvis-node-mobile. A mobile merge also builds and submits to TestFlight and the Play internal track, so the rule goes into that repo's `CLAUDE.md` before the loop works there. *As built (phase 6b):* the same gates for every active repo (both mobile apps, the docs, the installer, the 29 command and device packages); loopd's built-in hold rules cover each one's release and deploy config, native code, auth and pairing, secrets and install scripts, CI and deleted tests (a rule's repo may end in `*`: `alexberardi/jarvis-cmd-*`), and the review agent holds what a path cannot show (`needs_review`). Mobile still ships every merge to the testers (the user, 2026-10-15); package tags and Pantry submissions stay human.

## Security

- **Network:** `loopd` listens on the LAN only. A Cloudflare Tunnel can come later for remote access, with Cloudflare Access (the user's login) in front of it, so the evidence links never become public URLs.
- **On the prod box:** its own system user, data directory and port, a systemd unit, nothing shared with jarvisd. It reads nothing of jarvisd's and writes nothing outside its own directory. Nightly copy of the SQLite file and the evidence folder.
- **Tokens:** the Slack bot and app tokens, one bearer token per runner and one for the user, all in `loopd`'s env file (0600). Runners keep their own GitHub credentials; `loopd` never holds a GitHub token, so a compromised server cannot push or merge.
- **Who can approve:** only Slack member `U0C9AFPAMD4`. A press or reply from anyone else is refused and recorded.
- **What agents may do** is unchanged: the permission settings of the machine they run on, plus the merge rule. `loopd` adds limits; it grants nothing.

## Rollout

Each phase is usable on its own and proves one thing before the next builds on it.

1. **Repo and design.** This repo with this doc; jarvis-server's `CLAUDE.md` and harness docs point to it. *Proves:* the agreement.
2. **loopd core on the dev box.** Items, events, the stage rules, priority, the active-agents number, the API and the `loop` CLI, with stages moved by hand. *Proves:* the state machine and limits, with no agents involved.
3. **Slack.** Item threads, `/loop`, questions and answers, the buttons and reply approvals, the digest, the active-agents control. *Proves:* the pipeline runs from a phone.
4. **Runner and the build stage.** One runner on the dev box claims items and runs the build brief; landing still by hand. *Proves:* an unattended agent produces a PR with evidence.
   Then **4b, the loop's own harness** (`scripts/loop-sandbox`: real loopd, runner and bot code against a fake Slack, a fake claude and a fake GitHub, with scenario files and Slack transcripts), so the loop's own PRs meet the three gates. Then **the planning stages** in `#plan`: product and engineering personas, briefs and blueprints as pages, item dependencies. *Proves:* an idea becomes loop-ready items without hand-written specs.
5. **Triage and review.** The two agent stages around build; bounce and needs-you. *Proves:* bad fixes and stale reports get caught before the user sees them.
6. **Land.** The landing script, held merges through Slack. *Proves:* end to end without a human in the middle.
7. **Move to the prod box.** `loopd` runs there as its own service; the dev box becomes just a runner. *Proves:* the split, and survives the dev box rebooting.
8. **Nightly regressions, at 19:00 America/New_York.** The scenarios, admin, desktop and mobile shots on `main`, with failures filed as items. *Proves:* the loop finds work on its own.
   Then **6b, every active repo** (2026-10-15): routing triage, the runner's verify step, setup commands, repo aliases in Slack and the hold rules for the mobile apps, docs, installer and packages. *Proves:* an item about any of them goes from a Slack message to a merged PR with the same gates.
9. **Later:** a Mac runner, the Cloudflare Tunnel.

## Decisions (2026-10-14)

- LAN only, a tunnel later; this private repo, documented from jarvis-server; the review stage stays; runners work continuously.
- Work the loop files itself starts on its own and is worked by priority; priority is required, P1–P5.
- A digest every 4 hours; the nightly run at 19:00 America/New_York.
- Merge rule, every repo: an external agent's review (judging test coverage), harness verification with screenshots in the PR, and CI. The hold list stays on top; approval by button or a plain reply.
- New features go through planning first: `#plan`, a product persona (brief) then an engineering persona (feasibility, blueprint, loop-sized pieces), each approved by the user; briefs and blueprints are pages, with a final copy in the target repo.
- Plan usage is not readable by a runner: the user sets the active-agents number (0–6); spend against ~1/7 of a week is advice only. 45 minutes per run.
