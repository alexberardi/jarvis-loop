# jarvis-loop

The agent pipeline for the Jarvis repos: work enters, is reproduced, built, reviewed by a second
agent and landed under the merge rule, with a human in the loop only for questions and held merges,
through Slack.

- `loopd`: the control plane (Go + SQLite + a small page + an API).
- runners: daemons on the machines that do the work (GPU, emulator, Mac), each starting headless
  `claude -p` agents.
- `loop`: the CLI over the same API.

Design: [docs/DESIGN.md](docs/DESIGN.md) (the living version, with drawings, is linked at its top).
Status: rollout phases 1 (repo and design) and 2 (loopd core) done; phase 3 (Slack) in review.

Slack: `loopd --slack-env ~/.config/jarvis-harness/slack.env` (the file must be 0600) turns on item
threads in `#loop`, `/loop`, answers and approvals by reply or button, the active-agents control
and the 4-hourly digest in `#loop-digest` ([docs/phases/03-slack.md](docs/phases/03-slack.md)).
