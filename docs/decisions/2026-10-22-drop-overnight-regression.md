# The overnight run is dropped: opt-in only, and silent when no runner can take it (2026-10-22)

**Context (#505, after #416).** The user dropped the overnight regression on 2026-10-22: per-PR
verify already runs the pipeline scenarios, so a nightly full run adds little. But R1 (scheduled
with `@jarvis-loop regression tonight`) was still `scheduled`, a scheduled row never expired, and
no runner takes regression work, so every morning at 07:00 loopd posted "Overnight regression R1
has not started … no runner takes it" in `#loop` and recorded a `regression_missed`
([2026-10-21-overnight-missed-nights.md](2026-10-21-overnight-missed-nights.md)).

**Decision.** Keep the overnight suite as an opt-in (the manual `regression [web|mobile|all]`
trigger of [2026-10-18](2026-10-18-regression-trigger.md) is untouched), but it never speaks when
nothing could run it:

- **Off by default.** The control's `regression_nightly` stays off unless the user turns it on;
  nothing is scheduled on its own.
- **Nightly needs a taker.** With `regression_nightly` on, the claim path schedules a night only
  when a runner seen in the last day asks for regression runs and has every resource of the suite
  (emulator, gpu, voice: `pipeline.RegressionTaker`).
- **A run no runner can take expires unnoted.** A scheduled run still not started at the 07:00
  after its set time, with no runner seen that can take it (none seen counts), is cancelled by
  loopd (`regression_cancelled`, via `expired`): no note, no `regression_missed`, no carry-over.
  That is how the live R1 ends: at the first 07:00 after this deploys. A run a runner could take
  but the queue kept from starting still gets the 07:00 note and carries over, as before.
- `pipeline.NoRegressionTaker` (the note's "no runner takes it: …") is gone with the case it
  explained.

**Not done.** The overnight runner code (lanes, the morning report, the board's toggle) stays for
an opt-in run; deleting it is a bigger change the ask did not need. A `regression_nightly` the user
turned on in the live control is not reset; with no runner that can take the run it schedules
nothing.
