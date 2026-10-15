# Phase 4b: the loop's own harness

Goal: jarvis-loop can verify changes to itself the way jarvis-server does, so the loop's own PRs
meet the three merge gates (agent review, harness verification with evidence in the PR, CI). It
runs the whole pipeline together (real `loopd`, real runner, real Slack bot code) against fakes,
on temp data, without touching real Slack or GitHub and without using the user's plan.
Built right after phase 4 merges, from the dry-run pieces phase 4 already has (the fake `claude`,
the local loopd wiring). **Proves:** a change to the loop can be shown working end to end before
it lands.

**As built (2026-10-15):** `docs/decisions/2026-10-15-loop-harness.md` (the shape, the fake clock,
what is real and what is fake, findings). The scenario format is in "Scenario format" below.

## `scripts/loop-sandbox`

```
scripts/loop-sandbox up [--name N]        # temp loopd + runner + fake Slack + fake claude
scripts/loop-sandbox run <scenario>...    # play scenarios against it (default: scenarios/*.json)
scripts/loop-sandbox transcript [--item N] # render the Slack threads as they would have looked
scripts/loop-sandbox down [--name N] [--purge]
```

- Everything under `.sandbox/<name>/` (gitignored): loopd's data, the runner's work root and its
  bare mirrors of a scratch repo the sandbox creates, the tokens (0600), logs.
- Loopback only, a free port, no `--lan`. Never reads `~/.config/jarvis-harness/` or real tokens.
- **Fake Slack:** a small server speaking the Web API calls and Socket Mode the bot uses
  (`internal/slack`'s test fake, promoted to `internal/slackfake` so tests and the sandbox share
  it). It records every message and can *inject* user actions: a thread reply, a button press, a
  slash command, from the approver or from someone else.
- **Fake claude:** the phase 4 test binary, driven by a script per scenario step (write a result,
  hang, exit non-zero, emit a usage-limit event, try a write outside the worktree, ignore SIGTERM).
- **Fake GitHub:** the PR step is the runner's injectable PR client; in the sandbox it records the
  PR (number, branch, head) instead of calling GitHub.

## Scenarios (`scenarios/*.json`)

A scenario is a list of steps (create an item, inject a Slack action, script the next agent run,
advance by CLI for the stages not automated yet, wait for an event) and expectations: the item's
stage, specific events, and Slack messages (channel, thread, text matching a regexp, buttons
present), plus **must-not** expectations (no message outside the two channels, no token in any
message, no event from an unexpected actor). Starter set:

1. **Happy path:** intake → (CLI) triage → runner build → PR recorded → (CLI) review → land holds
   it (auth path) → approve by thread reply → echo with Cancel → 2-minute window (fake clock) →
   land → (CLI) merged → done. Expect each thread message in order.
2. **Cancel:** the same up to the echo, then Cancel → still held.
3. **Not an approval:** "looks fine?" → stays held, the bot's "not taken as an approval".
4. **Someone else:** another user replies "approve" and presses the button → refused, ephemeral,
   `refused` events, still held.
5. **Guard:** the agent tries to write outside its worktree → `guard_refused` event and a thread
   line; the agent then finishes inside → PR.
6. **Hang:** the agent hangs → the time cap → needs-you with "run exceeded".
7. **Stop:** `/loop stop` mid-run → the agent's group is killed, a WIP push to the item branch
   only, the item back in its queue.
8. **Usage limit:** the agent emits a usage-limit event → `usage_limit`, active agents 0, the
   control message shows 0, a digest-channel post with the reset text.
9. **Priority:** two ready items, P3 then P1 added → the runner takes P1 first.

## Transcript

`transcript` prints the recorded threads as plain text (and writes `transcript.md` into the run's
output dir): the parent line, each reply, buttons as `[Approve merge] [Keep holding]`. A PR that
changes message wording attaches the before/after transcripts as its evidence, the same idea as
screenshots in jarvis-server PRs.

## Optional live check

A `#loop-test` channel (the user creates it and invites the bot) for the rare change that must be
seen in real Slack. `loop-sandbox up --live-slack` connects to real Slack with the bot restricted to
`#loop-test` only (the allowlist swaps to that one channel). Never `#loop` or `#loop-digest`.

## Into the loop's own gates

- `CLAUDE.md` in this repo: a Harness section naming these commands; the build brief
  (`skills/build/SKILL.md`) says that for jarvis-loop items the harness evidence is the scenario
  results plus the transcript.
- CI also runs the scenarios (they are fast and need nothing real), so a scenario regression is red.

## Tests

The scenario runner itself: a scenario that should fail does (wrong stage, missing message,
a forbidden message), with a readable failure. The fake Slack's injection and recording. CI runs
`scripts/loop-sandbox run` headless.

## Scenario format (as built)

A scenario file is `{"name", "about", ["stages"], "steps": [...], "expect": {...}}`; `stages` are
the stages the runner claims (default triage, build and review, as a real runner). Items are named by an
alias (`"a"`) given when added; ids are the stack's own. Unknown fields are refused, so a typo
cannot silently check nothing. Steps (`"do"`):

| Step | Does |
| --- | --- |
| `add` `item`, `title`, [`priority`], [`via`: `cli`\|`slack`], [`text`], [`after`: aliases] | add an item (CLI: `POST /items` with repo `demo`; Slack: `/loop add <title>`, or `/loop add <text>` as typed, repo and priority words included); `after` makes it depend on those items (CLI: `depends_on`; Slack: `… after #1, #2`) |
| `triage` / `review` `item` | claim the stage by hand (a `hand` runner token, as `loop claim`/`loop advance`) and finish it: reproduced with evidence / approved for the PR head |
| `land` `item`, `outcome` (`hold`\|`merged`\|`back_to_build`), [`reason`] | the land stage by hand, with the PR's changed paths (a scenario whose `stages` include `land` lets the runner's land script do it instead) |
| `script` `item`, `play`, [`file`], [`for`], [`picks`] | how the fake claude plays the item's runs of a stage (`for`: `triage`, `build` (default), `review`). Build: `good`, `escape`, `hang`, `ignore_term`, `usage_limit`, `question`, `exit3`, …; `file`: the path it changes; after a bounce it addresses `.loop/review/`. Triage: `reproduce` (default; `picks`: the repo it names when the item has none), `duplicate`, `triage_question`, `triage_bad_repo`. Review: `approve` (default), `bounce`, `review_commit` |
| `runner` [`claims`: alias\|`none`] | the runner polls once: closes the PRs of dropped items (as `Run` does each poll), then claims (and works in the background) or finds nothing |
| `agent_running` `item` | wait until the item's agent is mid-run (a hanging fake) |
| `runner_idle` | wait until no run is working |
| `clock` `advance` (`"2m"`, `"45m"`) | move the fake clock, in 30-second steps |
| `reply` / `mention` `item`, `as` (`approver`\|`other`), `text` | a thread reply / mention in the item's thread |
| `press` / `choose` `item` or `message: control`, `button` (action_id), [`value`], `as` | press a button / pick a menu option on the newest message that has it |
| `command` `text`, `as`, [`answer` regexp] | `/loop <text>`; the ephemeral answer must match |
| `control` `active_agents`, `max_run_minutes`, `stopped` | `PUT /control` as the CLI |
| `ci` [`item`, `checks`: {name: `pass`\|`fail`\|`pending`\|`cancel`\|`skip`}], [`main`: {name: `pass`\|`fail`\|`pending`}] | the CI checks on the item's PR head now (a new head has none), and how the workflow runs on main's merge commits go (phase 6) |
| `push` `item` or `branch` (`main`), `file`, `text` | someone else's commit on the item's branch, or on demo's main |
| `github` [`refuse_merges`: n], [`item` + `merge_by_hand`], [`item` + `close_by_hand`] | GitHub refuses the next n merges; the item's PR is merged (or closed) on GitHub outside the loop |
| `expect` / `wait` + expectations | as below, at that point |

Expectations (`expect`, in a step or at the end) are retried for `within` (default 10 s); the
negative ones are checked once the loop has gone quiet:

| Field | Holds when |
| --- | --- |
| `stage`: {alias: stage}, `question`: {alias: regexp}, `leased`: {alias: bool}, `repo`: {alias: slug} | the item is there (its repo exactly; `""` for none) |
| `events` / `no_events`: [{`item`, `kind`, `actor` (`approver`, `other`, `runner`, `hand`, `user`, `loopd`), `outcome`, `data`: {key: regexp}, `count`}] | such events exist (exactly `count`) / none do |
| `messages` (in this order) / `no_messages`: [{`thread` or `parent`: alias, `channel` (`loop`\|`digest`), `from` (`bot`, `approver`, `other`), `kind` (`message`, `ephemeral`, `command`, `response`), `to`, `text` regexp, `buttons`: [labels], `pinned`}] | such messages exist, as they stand after edits / none do |
| `control`: {`active_agents`, `stopped`} | loopd's control |
| `prs`: n | PRs opened on the fake GitHub |
| `pr`: {alias: {`number`, `repo`, `title` regexp, `body` regexp, `edits`, `comments`, `comment` regexp, `state`, `merge_calls`}} | the PR the runner opened for the item, at its branch's head: how often it updated it, how many comments (review verdicts) it posted, the newest |
| `same_prefix`: [stages] | each stage ran at least twice, and every run's prompt starts with that stage's static brief and has the same argv |
| `remote`: {`branches`: {alias: head-subject regexp}, `files`: {alias: [paths]}, `only`, `main`: alias} | the remote's branches; `main` is where it started or where a merge or `push` put it, and (`main`) is that item's squash commit |
| `agent_runs`: {stage: n} | the runner started n agent runs for that stage (land starts none) |
| `absent`: [paths under the stack dir] | they do not exist |
| `board`: {alias: {`column`, `card` regexp}} | the item's card is in that column of the web board (read from the stack's board listener with a board token, through the `?t=` cookie flow), its HTML matching `card` |

**Always checked, after every step (must-not):** no bot call to a channel other than `#loop` and
`#loop-digest`; no token (Slack, loopd user, runner or hand) in any call to Slack or any message; the
bot token on every bot call and the app token on `apps.connections.open`; no `<!channel>`,
`<!here>` or `<!everyone>`, no mention of anyone but the approver; no message over 2,000
characters; no token in a PR's title or body; no event by an actor other than loopd, the approver, the sandbox user, the runner or the
hand runner, except `refused` events recording someone else's attempt.
