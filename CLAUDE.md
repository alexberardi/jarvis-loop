# jarvis-loop

The agent pipeline (`loopd`, runners, the `loop` CLI) for the Jarvis repos. **Read
[docs/DESIGN.md](docs/DESIGN.md) first**: stages, priority, the merge gates, usage control and the
rollout phases are settled there (2026-10-14). The living version, which the user comments on, is
linked at its top; when the design changes, update both.

## Rules

- **Go** like jarvis-server: `go` 1.27 on PATH (mise), **`CGO_ENABLED=0` for builds** (SQLite is
  `modernc.org/sqlite`), race tests with `CGO_ENABLED=1 go test -race ./...` before committing.
- **TDD**: the stage machine, priority order, limits and leases are pure logic and get table tests
  first. No sleeps in tests: inject the clock.
- **Secrets:** Slack tokens live in `~/.config/jarvis-harness/slack.env` on the dev box (later
  loopd's env file on the server). Never print them, never commit them, never put them in argv.
- **What may go to Slack** (a third party): summaries, PR links, small evidence. Never full logs,
  model transcripts, credentials, tokens, or host names from jarvis-server's `HOSTS.local.md`.
- **Never touch** the user's checkouts, the user's emulator (`emulator-5554`), or processes this
  code did not start.
- **Merging (since rollout phase 6, 2026-10-15):** the loop lands its own PRs. Work through the
  loop (`loop add`, Slack) rather than merging by hand: the land stage merges once review approved
  the head, CI passed on it and the build's evidence is in the PR, and holds for the user when
  `loopd`'s hold rules match (`pipeline.DefaultHoldRules`: the files for who may approve or merge,
  the hold rules and the control row, API auth and acting for the user, the Slack approver check,
  tokens, the land script, the trusted files, the guard and sandbox, CI, removed tests or scenarios;
  the rest of `internal/pipeline`, `internal/api` and `internal/store` lands on review and CI, so
  keep approval, auth and token code in its own held files:
  [decision](docs/decisions/2026-10-18-narrow-loop-holds.md)). A held PR merges after the user's approval in its Slack thread. A PR made by
  hand outside the loop: squash-merge your own once every CI check passed (wait explicitly; never
  `gh pr merge --auto`), then check main's CI, and hold for the user under the same rules or when it
  touches the prod box. Never edit the hold rules to get a PR through; that change is itself held.
- **Decisions** get a file each in `docs/decisions/YYYY-MM-DD-<slug>.md`; no session log (history is
  the commits and PRs), as in jarvis-server.

## Harness

The loop's own harness (`scripts/loop-sandbox`, [docs/phases/04b-loop-harness.md](docs/phases/04b-loop-harness.md))
runs the real loopd wiring, runner, Slack bot and `loop-guard` against a fake Slack, a fake claude
and a fake GitHub, on temp data under `.sandbox/<name>/` (gitignored), loopback only. It never reads
`~/.config/jarvis-harness/` or `~/.config/jarvis-loop/`, never uses port 7780, and never talks to
real Slack or GitHub.

- `scripts/loop-sandbox run [scenarios/x.json ...]`: play the scenarios (default `scenarios/*.json`),
  each on a fresh stack with a fake clock; about 7 s for all. Results and one `transcript.md` per
  scenario land in `.sandbox/<name>/runs/latest/`. CI runs it too, and `go test ./internal/sandbox`
  plays the same files.
- `scripts/loop-sandbox transcript [--scenario S] [--item N]`: the Slack threads as they would have
  looked.
- `scripts/loop-sandbox up` / `down [--purge]`: a long-lived sandbox on the real clock to poke by hand
  (`source .sandbox/default/live/loop.env && .sandbox/default/bin/loop status`,
  `scripts/loop-sandbox inject reply --item 1 approve`, `transcript --live`).

**A change to the loop shows its evidence the way jarvis-server's do:** the scenario results, plus
the transcript(s) of what it changes; a change to message wording attaches the before/after
transcripts. New behaviour gets a scenario (format: the "As built" section of the 4b spec).
