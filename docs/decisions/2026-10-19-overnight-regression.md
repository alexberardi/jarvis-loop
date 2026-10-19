# The overnight regression: starts when the queue is quiet, reports in the morning (2026-10-19)

Item #152, E2E 12/12 of the end-to-end regression plan (the user, 2026-10-17): the full automated
regression of web and mobile, run by hand (#149, [2026-10-18-regression-trigger.md](2026-10-18-regression-trigger.md))
or overnight after a bulk of work. The user's decisions: the scripted model for the UI suites, the
real model only in the pipeline scenarios and the voice lane; a manual trigger plus an overnight
run; camera and biometrics stay manual. This piece is the overnight run.

## Decided

- **A fifth suite, `overnight`**, scheduled only (`regression overnight` by hand is not a
  command; `pipeline.Suites` stays web, mobile, all). It needs the runner's `emulator`, `gpu` and
  `voice` capabilities (`SuiteResources`): only the dev box's runner takes it.
- **Scheduling:** `@jarvis-loop regression tonight` (the approver's only, in
  `approve_regression.go`; `regression tonight off` cancels), the board's **Run overnight
  tonight** toggle (the user's token only, a form post to `POST /board/regressions/tonight`
  under the same cross-origin guard as Run regression; pressed again it cancels), and the API's
  `POST|GET|DELETE /regressions/scheduled` (user token). The run is a `regressions` row with status
  **`scheduled`** (migration 0028; at most one, its own unique index) and its set time
  `not_before`: the next `regression_at` on the loop's clock (America/New_York). A second ask
  while one is scheduled is refused (`regression_open`) with the time it waits for. A scheduled run
  is not an *open* run: a manual `regression web` in the day still runs, and the overnight one
  waits while any run is open.
- **Every night:** the control's `regression_nightly` (bool) and `regression_at` (`HH:MM`, default
  01:00), set with `PUT /control`. With it on, the claim path schedules one (via `nightly`, actor
  loopd) whenever none is, once a night: a set time with an overnight run started in the 12 hours
  before it (or after it) moves to the next night. Turned off, the nightly-scheduled run is
  cancelled; one the approver scheduled stays.
- **The quiet gate** (`pipeline.QueueBusy`, `OvernightReady`): the run starts once it is at or past
  its set time **and** the queue has been quiet for 10 minutes, whichever is later. Quiet: no live
  lease of an agent stage (triage, build, review, a plan turn, the orchestrate turn: all of them, not
  only build/review/verify, since each holds the CPU and the sandbox ports a verify needs) and
  nothing an agent could start: no item waiting in triage, build or review whose dependencies are
  merged, that a frozen repo does not hold and some runner offers the resources for; no queued plan
  turn; no orchestrate turn that may start. Land is not "not quiet": it needs no emulator and
  lands through the night (decision 2026-10-18-land-never-waits). The claude pace is not looked
  at: work it holds back still waits, and the night is the queue's first. The quiet clock is the
  row's `quiet_since`, kept by every runner's claim (any runner, any kinds), and reset when the
  queue is busy. If the queue never goes quiet the run waits, into the day if need be; the toggle or
  `regression tonight off` cancels it.
- **The hold:** while an overnight run's lease is live (`LiveLease.Holds`), a claim gets only land:
  no item, plan turn or orchestrate turn, not even a frozen repo's fix (the run has the emulator,
  the sandbox ports and the CPU; the user is asleep). A manual run holds nothing, as before.
- **A lease that ends early** (its runner went away, the loop stopped) puts the run back in the
  queue as `queued`, as a manual run's: it already passed the gate, and starts again at the next
  free slot.
- **Its cap:** `pipeline.OvernightMaxRun`, **6 h** (three lanes and the reruns), instead of the
  3 h of a manual run. The runner gives each lane at most `regression.timeout` (150 m) and stops
  starting lanes and reruns 30 minutes before loopd's cap, a lane not run being the run's error.
- **The lanes, in one claim** (`internal/runner/overnight.go`), in the run's checkout, under the
  verify lock and the verify steps' fence, each on a sandbox of its own (`loop-reg-<id>-<n>`):
  1. `scripts/e2e all [--mobile-repo]` (web and mobile, the scripted model);
  2. `scripts/e2e voice` (the voice lane, the real model through the tap, `--audio wav`);
  3. the pipeline scenarios: `scripts/sandbox up --name N`, `go run ./tools/pipeline run
     --sandbox N --json` (its summary in the run dir), `scripts/sandbox down --name N --purge`.
     jarvis-server has no `scripts/e2e pipeline`, so the runner reads tools/pipeline's JSON summary
     (name, passed, runs, failures, skipped) as tests of lane `pipeline` (failed unless every
     run passed; the error is its failures). `regression.pipeline_command` replaces this command
     (the loop's harness points it at a stand-in).
- **Flaky:** scripts/e2e has no retry of its own (only the mobile runner's transport retry), so
  after each lane the runner reruns its failures once, a suite at a time, with `--filter` (the
  titles, regexp-quoted; for web the test's name after the file's, as Playwright greps the full
  title; for pipeline, tools/pipeline's own `--filter`). A failure that passes on the rerun gets the
  status **`flaky`** (its error the first run's, "first run: …"): counted apart (`flaky`), never
  a failure, no File it, and an e2e proof check counts it as passed (the QA verifier does not reopen
  an item on it). One that fails again keeps its first run's error and screenshot.
- **Recordings:** a failure's first attachment or artifact that is audio or video (a voice case's
  `reply.wav`) is uploaded like its screenshot (`regression_tests.recording`); Slack and the board
  link it.
- **The report in the morning:** a finished overnight run gets `report_at`
  (`pipeline.MorningReport`): at once when it ends between 08:30 and 20:00 on the loop's clock,
  else the next 08:30 (before the 09:00 standup). The `regression_done` event carries it; the bot
  skips the event and its tick posts the due reports, each once (`reported_at`; one Slack refuses
  is tried on the next tick). The message: the verdict, totals with flaky apart, the duration (from
  the claim), the heads, the board's link; the totals per lane (web, mobile, pipeline, voice); each
  failure with its assertion, its screenshot and recording links and File it; then a **Flaky**
  line listing the tests that passed on the rerun; and the QA line (#195) as before. The board's
  report page has a Flaky section, the recordings and the duration.

## Left open

- The real lanes have not run under the loop's fence on the dev box: whether the voice lane and
  the pipeline scenarios reach the installed jarvisd's model through the tap from inside the fence,
  whether `go run ./tools/pipeline` builds there (its module cache), and how long a full night
  takes. The harness runs stand-ins with the real formats; the user's first overnight run is the
  check.
- The pipeline scenarios' recordings (`--out`'s `.wav` per voiced turn) are not uploaded: the
  summary does not name them.
- Per-PR runs.
