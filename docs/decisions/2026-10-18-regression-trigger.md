# The regression trigger: a run from Slack or the board, run by a runner, reported in #loop (2026-10-18)

Item #149, E2E 9/9 of the end-to-end regression plan (the user, 2026-10-17): the full automated
regression of the admin web app and the mobile app, on a manual trigger. jarvis-server's
`scripts/e2e [web|mobile|all]` (pieces 1 to 8) runs the suites and writes `.e2e/<run>/report.json`
(schema 1, its `docs/dev/e2e.md`); this piece starts it from the loop and reports it. Not run per
PR, nor nightly yet (#152 builds on this).

## Decided

- **A fourth claim kind, `regression`** (`pipeline.WorkRegression`), opted into by
  `ClaimRequest.Kinds` like the plan and orchestrate turns: a run is not an item (it would sit in
  the board's columns and go through stages it has none of). Its own table, `regressions`
  (migration 0022), holds the run and its lease, like `orch_turns`.
- **The trigger:** `@jarvis-loop regression [web|mobile|all]` (all when it names none; voice is left
  out: it takes the real model and minutes, by hand only), the **approver's only**
  (`slack.IsApprover`, in `approve_regression.go` so the `slack-approver` hold rule covers it);
  someone else's is refused like any other approver action. Only those exact words are the command:
  "regression in the inbox: …" is still an item to add. The acknowledgement is ephemeral, to the
  approver only. On the board, a **Run regression** button with the suite, shown and accepted only
  when the board was opened with the **user's token** (a board token gets 403): a form post to
  `POST /board/regressions`, guarded by `http.CrossOriginProtection` as the status page's control
  form is (decision 2026-10-14-web-page-auth), the board page's CSP then allowing `form-action
  'self'`. It is the board's only write. The API has `POST /regressions` (user token) too.
- **One run at a time:** while one is queued or running, a second ask is refused
  (`regression_open`, 409 on the API) with the run it waits for. Not queued behind it: the user
  asks again when it ends.
- **It counts as an agent:** a running run is a live lease of stage `regression` that takes one of
  the active-agents slots. A queued run goes to the next free slot after the orchestrate turn and
  before every item and plan turn (the user asked for it now). The `mobile` and `all` suites need
  the **`emulator`** resource: only a runner offering that capability takes them, and they count
  against the emulator's limit (1) as an emulator item does, so a mobile build's verify and the run
  never share the emulator. A slot held for emulator work (`SlotHolds`) may go to a mobile run.
- **The lease:** heartbeats as any run's; an expired lease, or the loop stopped, puts the run back
  in the queue (nothing was reported). Its cap is **`pipeline.RegressionMaxRun`, 3 h**, not the
  control's `max_run_minutes` (sized for an agent): past it, the run ends with an error. The
  runner's own limit (`regression.timeout`, default 150 m) ends it first.
- **The runner runs it, no agent** (`internal/runner/regression.go`), stage `regression` in its
  `stages` and a `regression` section in its config: `repo` (the jarvis-server key) and
  `mobile_repo` (jarvis-node-mobile's; required when the runner offers the `emulator` capability, so
  scripts/e2e never looks for the app outside the run's checkouts). It checks both out fresh at
  their base from its mirrors (the mobile app only for mobile/all), runs the server repo's prepare and setup commands, takes the
  machine's **verify lock** (one emulator, one GPU), and runs
  `scripts/e2e <suite> --name loop-reg-<id> [--mobile-repo <checkout>]` in the **same fence as the
  verify steps** (it needs KVM, the harness cache and `~/.android`, which no agent sandbox has; the
  secrets stay masked). It never uses `--name default`, never touches the user's checkouts or
  emulator.
- **The report:** the runner reads the newest `.e2e/<run>/report.json` (schema 1 only), uploads
  each failure's first screenshot (a path inside the run's dir only, at most 40), `report.md` and
  `report.json` to `POST /regressions/{id}/files`, and sends the tests
  (`POST /regressions/{id}/result`): suite, title, file, line, status, the assertion (`error`), its
  screenshot, and the repo to file it in (web: the server repo, mobile: the app's). A suite that did
  not run (status `error`) is the run's error next to its tests. No report at all is a run that
  **could not run**, with scripts/e2e's exit code and its last three lines. loopd keeps the tests
  (`regression_tests`) and files (`regression_files`, under its data dir); statuses `passed`,
  `failed` (a test failed, or a suite did not run) and `error`.
- **One message in #loop** when a run ends (event `regression_done`): the verdict and counts, the
  heads it ran, a link to the full report on the board (`/board/regressions/<id>`), then each
  failure (at most 15; the board has them all) with its name, its assertion and a link to its
  screenshot. **Slack gets links to the screenshots, not the images:** the bot has no file-upload
  scope, and adding one is an access change of its own. The links need loopd's `--link-base` (the
  LAN address); without it the message says where the report is.
- **File it:** a button per failure (approver only). It files an item in the failure's repo: title
  `E2E failing: <test>`, P2, source `finding`, the ask with the assertion, the heads and the
  report's address, and the screenshot copied in as the item's evidence. **Deduplicated by test**
  (`suite: title`, `regression_items`): while an item filed for the same test is open (not done or
  closed), pressing again, or on a later run's report, gives that item ("Already open as #N")
  instead of a second one. The report message is updated to show "Filed as #N" in place of the
  button, and a later run's report of the same failing test shows the open item from the start.
  The API's `POST /regressions/{id}/tests/{n}/file` does the same for the user's token.
- **The board:** a Regressions panel (the last five runs, the open one, the button) above the lanes,
  and each run's report page: counts, heads, the failures with their assertions and screenshots,
  every test, and the files.
- **The orchestrator** does not see a run as a change: its fingerprint leaves the `regression_*`
  events out, as it leaves out its own turns'.

## Left open

- Per-PR runs. Nightly runs were built in #152 ([2026-10-19-overnight-regression.md](2026-10-19-overnight-regression.md)). The QA verifier reads the results since #195: a finished run
  decides the e2e checks of the items in qa ([2026-10-18-qa-proofs.md](2026-10-18-qa-proofs.md)).
- No `loop regression` CLI command: Slack, the board and the API cover the trigger.
- The real `scripts/e2e` has not run under the loop's fence on the dev box (the harness runs a
  stand-in with the same report format): the first real run is its check.
