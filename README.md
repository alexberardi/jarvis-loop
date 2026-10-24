# jarvis-loop

The agent pipeline for the Jarvis repos: work enters, is reproduced, built, reviewed by a second
agent and landed under the merge rule, with a human in the loop only for questions and held merges,
through Slack.

- `loopd`: the control plane (Go + SQLite + a small page + an API).
- runners: daemons on the machines that do the work (GPU, emulator, Mac), each starting headless
  `claude -p` agents.
- `loop`: the CLI over the same API.

Design: [docs/DESIGN.md](docs/DESIGN.md) (the living version, with drawings, is linked at its top).
Status: rollout phases 1–4b (repo and design, loopd core, Slack, the runner and the build stage, the
loop's own harness) done; phase 5 (triage and review agents) in review.

Slack: `loopd --slack-env ~/.config/jarvis-harness/slack.env` (the file must be 0600) turns on item
threads in `#loop`, `/loop`, answers and approvals by reply or button, the active-agents control
and the 4-hourly digest in `#loop-digest` ([docs/phases/03-slack.md](docs/phases/03-slack.md)),
and plan threads in `#plan` (`/loop plans`, [docs/phases/07-planning.md](docs/phases/07-planning.md));
the ⏳/✅ reactions there need the app's `reactions:write` scope, and are skipped (logged) without it.
The `@orchestrator` app ([docs/slack/orchestrator-manifest.yml](docs/slack/orchestrator-manifest.yml),
[docs/phases/08-orchestrator.md](docs/phases/08-orchestrator.md)) runs beside it, on its own socket,
when `orchestrator-slack.env` (0600, the same two keys, the second app's tokens) sits next to the
`--slack-env` file, or `--orchestrator-slack-env FILE` names one (`off` turns it off).

Repos: `loopd --repos alexberardi/jarvis-loop,alexberardi/jarvis-server` names the repos the loop
works on: triage may give an item without a repo only one of these, and `/loop add` recognises
them by name ("in jarvis-loop", "jarvis-loop:", "[jarvis-loop]").

Board: `loopd --board-addr 0.0.0.0:7790 --slack-workspace https://<team>.slack.com` adds a second
listener for the LAN that serves only the read-only swim-lane board (`GET /board`), so the API stays
on 127.0.0.1. Add a `board` token to the tokens file (`token<TAB>board<TAB>phone`; the API refuses
it everywhere) and open `http://<host>:7790/board?t=<token>` once: the page keeps the token in an
HttpOnly cookie and drops it from the address. Why: [docs/decisions/2026-10-15-board.md](docs/decisions/2026-10-15-board.md).

Runner (phases 4 and 5, [docs/phases/04-runner-build.md](docs/phases/04-runner-build.md),
[docs/phases/05-triage-review.md](docs/phases/05-triage-review.md)):
`loop-runner --config ~/.config/jarvis-loop/runner.json` (0600) claims `triage`, `build` and `review`
items (config `stages`) and runs one `claude -p` per item in a working directory made from its own
bare mirrors under `~/.cache/jarvis-loop/work`, under
deny rules, the auto-mode classifier, the `loop-guard` hook and Claude Code's OS sandbox. `--once`
works one item; `--dry-run` skips the GitHub PR check, for runs with `tools/fakeclaude`. What is
allowed and why: [docs/decisions/2026-10-15-runner-choices.md](docs/decisions/2026-10-15-runner-choices.md).
A repo with a `harness` (`{"up", "down", "timeout", "repo"}`) lets its build and review agents run it
through the runner with `loop-harness up | run '<cmd>' | down | status`; install `loop-harness`
next to `loop-runner` (or set `loop_harness`). With `"repo"` (another repo's config key) the
session's commands run in a fresh checkout of that repo instead of the agent's worktree, which is
writable beside it as `$LOOP_WORKTREE`: jarvis-server's `scripts/e2e mobile` for a
jarvis-node-mobile agent. That and the repo's `verify` steps are the only ways to the emulator, so
an item whose `resources` include `emulator` needs one of them on its repo. Why:
[docs/decisions/2026-10-16-harness-broker.md](docs/decisions/2026-10-16-harness-broker.md),
[docs/decisions/2026-10-23-emulator-claims.md](docs/decisions/2026-10-23-emulator-claims.md).
With `"land"` in `stages` (phase 6, [docs/phases/06-land.md](docs/phases/06-land.md)) it also lands
approved items itself, with no agent: the gates from GitHub and git, `gh pr merge --squash
--match-head-commit`, then main's CI on the merge (`land_ci_wait`, `land_main_wait`, default
`"30m"`). Why: [docs/decisions/2026-10-15-land.md](docs/decisions/2026-10-15-land.md). Land has
lanes of its own, `land_slots` (default 3) next to the `slots` agent runs, so an approved item never
waits for a build or for another item's CI ([docs/decisions/2026-10-16-land-lane.md](docs/decisions/2026-10-16-land-lane.md),
[docs/decisions/2026-10-18-land-never-waits.md](docs/decisions/2026-10-18-land-never-waits.md)).
With `"plan"` in `stages` (phase 7, [docs/phases/07-planning.md](docs/phases/07-planning.md)) it
also takes plans' turns: the product and engineering personas, one short agent per turn in
`<work_root>/plan-<id>/`, resuming the persona's session (`plan_turn_minutes`, default 15).
With `"orchestrate"` in `stages` and loopd's orchestrator token in `orchestrator_token` (phase 8,
[docs/phases/08-orchestrator.md](docs/phases/08-orchestrator.md)) it also takes the orchestrator's
turns: one short read-only agent per turn in `<work_root>/orchestrator/`, resuming the day's session,
whose actions the runner sends to loopd with that token. Why: [docs/decisions/2026-10-17-orchestrate-turns.md](docs/decisions/2026-10-17-orchestrate-turns.md).
With a `qwen` block (`base_url`, `credentials_file`, `model`, `max_minutes`, `opencode`), build runs
of items whose plan piece says `builder: qwen` run OpenCode in the verify fence against jarvisd,
through a proxy in the runner that adds the app credentials; the second bounce turns the item to
claude. Why and the gaps: [docs/decisions/2026-10-16-qwen-builder.md](docs/decisions/2026-10-16-qwen-builder.md).
A config change needs no restart: `loop-runner reload` (SIGHUP) reads `runner.json` again for the
next claim (repos, slots, stages, verify steps); live runs keep theirs, and a bad file is refused
with a log line. To update the binary, `loop-runner drain` (SIGUSR1): it claims nothing new, the
board shows it draining (and logs what it still waits on every 30 s), and it exits 75 once its
live runs end, so a service that restarts on failure (systemd `Restart=on-failure` or `always`,
launchd `KeepAlive`) starts the new binary. loopd reads `--repos-file` again on
SIGHUP. Why: [docs/decisions/2026-10-18-runner-reload-drain.md](docs/decisions/2026-10-18-runner-reload-drain.md).
Both find the runner by `loop-runner.pid` in the work root. A second runner on the same work root
refuses to start while that file names a live loop-runner, and a runner removes the file only while
it holds its own pid. `loop-runner version` prints the commit the binary was built from; any other
subcommand is an error.

Harness (phase 4b, [docs/phases/04b-loop-harness.md](docs/phases/04b-loop-harness.md)):
`scripts/loop-sandbox run` plays `scenarios/*.json` against the real loopd wiring, runner, bot and
`loop-guard`, with a fake Slack, claude and GitHub, and writes each scenario's Slack transcript.
