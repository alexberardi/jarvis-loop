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
