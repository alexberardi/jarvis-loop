# The loop's own harness (phase 4b)

2026-10-15 · phase 4b (`scripts/loop-sandbox`, `internal/sandbox`, `internal/slackfake`,
`scenarios/`). Where the spec left room, and what the harness found on its first runs.

## Shape

- **One process plays a run.** `scripts/loop-sandbox run` builds `loop-sandbox`, `loop-guard`, the
  fake claude and `loop` into `.sandbox/<name>/bin`, then plays each scenario on a **fresh stack**
  inside the `loop-sandbox run` process: loopd's store and API on a free loopback port (real HTTP),
  the Slack bot and Socket Mode client against `internal/slackfake` (real HTTP and WebSocket), the
  real runner, a fake GitHub, and a scratch remote. A fresh stack per scenario keeps them
  independent: a usage-limit scenario leaves active agents at 0, a stop scenario leaves an item in
  the build queue, and either would steer the next scenario's claims. Item ids restart at 1 per
  scenario. The whole run (9 scenarios) takes about 7 s. Everything that listens is started and
  reached by that one command, so it should also run inside Claude Code's sandbox, where each Bash
  call has its own loopback (`2026-10-15-claude-headless-facts.md`); **not verified yet** with a
  real agent.
- **Real loopd wiring.** `cmd/loopd`'s handler and Slack start-up moved to `internal/loopd`
  (`Handler`, `StartSlack`), which loopd and the sandbox both call; tokens are loaded from a 0600
  file with `api.LoadTokens`, and the runner's config from a 0600 file with `runner.LoadConfig`, as
  the binaries do. What is not the binaries': the sandbox does not exec `loopd` and `loop-runner`,
  because the fake clock has to be shared in-process (below). Their `main`s are thin.
- **`up` is for poking by hand**, not for `run`: a long-lived stack on the real clock (`runner.Run`
  polling every 2 s, the 2-minute window in real time), with `live/loop.env` (the sandbox user's
  token, for the `loop` CLI), `live/hand.env` (a hand runner's, for `loop claim`/`advance`), and
  `inject reply|press|command` and `transcript --live` through a small token-checked control
  endpoint on loopback. `down` signals only the pid it started, after checking its command line.

## The fake clock

- One `runner.FakeClock` is loopd's clock (store, leases, the bot's approval window and digest) and
  the runner's (heartbeats, the time cap, the kill grace). Nothing moves it but a `clock` step.
- The bot's timer loop got `Config.TickEvery` (default 5 s, unchanged in loopd); the sandbox runs it
  every 20 ms, so a moved clock takes effect at once. The 2-minute reply-approval window is the
  real 2 minutes, passed on the fake clock.
- `clock` steps advance in 30-second steps and, between steps, wait until each working run has
  re-armed its two timers, as heartbeats would on the wall clock; otherwise a 45-minute jump would
  expire the lease (5-minute TTL) before the runner's cap fired. The hang scenario passes the real
  45-minute cap this way in under a second.
- On the fake clock loopd's once-a-minute sweep ticker does not run (every API request sweeps
  anyway, as in loopd).
- **The runner claims only on a `runner` step** (its poll loop is not run): scenarios decide when a
  claim happens, so "P3 then P1 are ready, the runner takes P1" does not race the poll.

## What is fake, and how

- **Slack:** `internal/slackfake`, promoted from `internal/slack`'s test fake (whose tests now use
  it through a thin adapter). It keeps a model of the workspace (messages, thread replies, edits
  with history, ephemerals, pins, slash commands typed and their answers), answers
  `message_not_found` for an update of an unknown message and `already_pinned` as Slack does, records
  each call's Authorization header (never rendered), and injects a person's reply, mention, button
  press, menu choice and slash command over the live Socket Mode connection, waiting for the ack.
  A press uses the button's own `value` and `block_id` from the posted message, and refuses a
  button the message does not have.
- **claude:** the phase 4 fake, which runs the generated settings' hooks, so every tool call goes
  through the **real `loop-guard`** (the built binary in `run`; the test binary in `go test`, as in
  the runner's tests). New: a per-item script (`FAKE_CLAUDE_SCRIPTS/<id>.json`: what to play, which
  file to change) and progress markers (`<id>.state`, written by the fake itself, not as a tool
  call: they are harness plumbing).
- **GitHub:** the remote is a bare repo the runner pushes the item's branch to with git, as it
  would to GitHub (`docs/decisions/2026-10-15-runner-pushes.md`: agents hold no GitHub
  credentials; the runner pushes and opens the PR). The runner's PR client (`Runner.PRs`: find,
  create, edit) and its PR check (`Runner.VerifyPR`) are a fake that records each PR (number from
  101, head branch and sha, base, title, the body the runner wrote with its Verification footer).
  So the sandbox runs the runner **with** its push, PR and PR check, not as a dry run; the PRs and
  their bodies are in each scenario's `prs.json` and `transcript.md`.
- **The stages not automated yet** (triage, review, land) are done by a `hand` runner token through
  loopd's API, the same calls `loop claim` / `loop advance` make; land passes the changed paths from
  the item's `pr_opened` event, as the landing script will.

## Checks

- Expectations are retried until they hold (Slack posts and runner reports are asynchronous);
  `no_events` / `no_messages` are checked only once loopd and the fake Slack have been quiet for
  150 ms, as they would otherwise hold vacuously before anything was posted.
- `messages` match in order of posting, against each message as it stands after its edits (so a
  mid-scenario `expect` sees the echo with its Cancel button, the final one sees "Applied.").
- The must-not checks run after every step: see the spec's "Scenario format".

## Spec deviations

- **Hang:** the spec says needs-you "with 'run exceeded'". By design (phase 4) the runner's own cap
  fires 30 s before loopd's overtime sweep, so the question is "the run hit the time cap (45
  minutes); work so far is on loop/…"; the scenario also asserts there is **no** `run_overtime` or
  `lease_expired` event.
- **`--live-slack`** (optional) is not built: `#loop-test` does not exist yet.

## Found by the harness

1. **A usage limit was never announced in `#loop-digest`** (DESIGN.md, Usage control: "posts the
   error with its reset time in `#loop-digest` with an @mention"): only the thread line and the
   control message changed. Fixed: the bot posts it on the `control` event with reason
   `usage_limit` (`TestUsageLimitIsAnnouncedInTheDigestChannel`, and the usage-limit scenario).
2. **Items added from Slack (`/loop add`, a mention) have no repo**, and a build runner fails such an
   item ("runner … has no repo \"\""). Not fixed here: whether triage sets the repo, the add
   command takes one, or the runner has a default is a design choice. The scenarios add items with
   the CLI (`--repo`), as phase 4's real runs do.
3. **`-race` helper binaries sleep 1 s at exit** (`GORACE` `atexit_sleep_ms`, default 1000), and
   each agent run starts the test binary several times (the fake claude, a guard per tool call).
   The runner's and the sandbox's tests set `GORACE=atexit_sleep_ms=0` for them: the runner's race
   tests went from 66 s to 5 s.
