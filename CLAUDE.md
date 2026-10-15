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
- **Merging (until the loop lands its own PRs, rollout phase 6):** squash-merge your own PR once
  every CI check has passed (wait explicitly; never `gh pr merge --auto`), then check main's CI.
  Hold for the user instead when a PR changes who may approve or merge (the approver check, token
  handling, the landing gates) or touches the prod box.
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
