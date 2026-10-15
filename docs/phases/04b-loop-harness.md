# Phase 4b: the loop's own harness

Goal: jarvis-loop can verify changes to itself the way jarvis-server does, so the loop's own PRs
meet the three merge gates (agent review, harness verification with evidence in the PR, CI). It
runs the whole pipeline together (real `loopd`, real runner, real Slack bot code) against fakes,
on temp data, without touching real Slack or GitHub and without using the user's plan.
Built right after phase 4 merges, from the dry-run pieces phase 4 already has (the fake `claude`,
the local loopd wiring). **Proves:** a change to the loop can be shown working end to end before
it lands.

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
