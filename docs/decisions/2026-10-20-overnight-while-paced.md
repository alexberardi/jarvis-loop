# The overnight run starts while the pace holds work back; a note at 07:00 says why it did not (2026-10-20)

Item #317. Amends [the overnight regression](2026-10-19-overnight-regression.md).

## What happened

On 2026-10-20 the 03:00 nightly never started. The quiet gate (`pipeline.QueueBusy`) counted every
ready item and ignored claude's pace on purpose ("work it holds back still waits"). Until 04:10 the
5h limit paused claude; from 04:45 the weekly budget's warning set the pace to slow (P1, size S and
triage only) and held back eight ready P2 items. The queue was busy and idle at once: no builds and
no regression run.

## Decision

- **The quiet gate looks at the pace.** `QueueBusy` takes the control and the pace and counts an
  item only when the pace lets it through, with the slow pace's starved fallback
  (`pacing.fallback`, [#316](2026-10-20-pacing-never-starves.md)): the same check the board makes
  to label an item paced or paused, so the board and the quiet clock agree. Plan turns and the
  orchestrate turn count only when claude may claim them (a turn that is not P1 waits under any
  pace but normal), as `chooseWork` does. The regression's lanes (`scripts/e2e`, the voice lane, the
  pipeline scenarios) are no claude runs, so pacing does not apply to them.
- **Paused:** ready claude items leave the queue quiet; the run starts at its set time plus 10
  quiet minutes. A qwen-routable build that a runner with a qwen block could take still counts.
- **Slow:** an item the pace lets through keeps the queue busy, and so does any ready item while
  the starved fallback would claim it (fewer than max(1, agents/3) claude runs live). So under a
  slow pace the regression still waits for the fallback's builds; the queue goes quiet only once
  the fallback's cap is reached and those runs end. Giving the night's run priority over the
  fallback's builds would change #316 and is left to the user.
- **The 07:00 note.** A scheduled overnight run still not started at 07:00 (the first 07:00 after its
  set time, on the loop's clock) gets one message in `#loop`: what keeps the queue busy (the live
  lease, the item, plan turn or orchestrate turn `QueueBusy` names), another open run, or a quiet
  queue no runner has polled since, plus claude's pace when it is not normal. The row's
  `unstarted_noted_at` (migration 0037) keeps it to one per run; a message Slack refuses is tried
  again on the next tick.
