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
and the 4-hourly digest in `#loop-digest` ([docs/phases/03-slack.md](docs/phases/03-slack.md)).

Repos: `loopd --repos alexberardi/jarvis-loop,alexberardi/jarvis-server` names the repos the loop
works on: triage may give an item without a repo only one of these, and `/loop add` recognises
them by name ("in jarvis-loop", "jarvis-loop:", "[jarvis-loop]").

Runner (phases 4 and 5, [docs/phases/04-runner-build.md](docs/phases/04-runner-build.md),
[docs/phases/05-triage-review.md](docs/phases/05-triage-review.md)):
`loop-runner --config ~/.config/jarvis-loop/runner.json` (0600) claims `triage`, `build` and `review`
items (config `stages`) and runs one `claude -p` per item in a working directory made from its own
bare mirrors under `~/.cache/jarvis-loop/work`, under
deny rules, the auto-mode classifier, the `loop-guard` hook and Claude Code's OS sandbox. `--once`
works one item; `--dry-run` skips the GitHub PR check, for runs with `tools/fakeclaude`. What is
allowed and why: [docs/decisions/2026-10-15-runner-choices.md](docs/decisions/2026-10-15-runner-choices.md).

Harness (phase 4b, [docs/phases/04b-loop-harness.md](docs/phases/04b-loop-harness.md)):
`scripts/loop-sandbox run` plays `scenarios/*.json` against the real loopd wiring, runner, bot and
`loop-guard`, with a fake Slack, claude and GitHub, and writes each scenario's Slack transcript.
