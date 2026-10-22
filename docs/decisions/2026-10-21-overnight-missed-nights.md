# An overnight run gets a note every night it misses, and says when no runner can take it (2026-10-21)

**Context (#416, reopening #317).** Overnight regression R1 (set for 01:00 on Oct 20) was never
claimed and carried over to the next night. Its 07:00 note went once, at deploy time (the first
07:00 after its set time had already passed, so it went at once), and never again: the note was one
per run (`unstarted_noted_at`, migration 0037). The note also said "the queue has been quiet …; it
starts after 10m of quiet" every time, though nothing could start it: the claim gives the overnight
run only to a runner that asks for kind `regression` and has every resource of the suite (emulator,
gpu, voice), and loopd did not record the kinds a runner asks for, so it could not tell.

R1 being the first regression run of any suite on this loopd points at no runner able to take it
(no `regression` in the runner's `stages`, or no `voice`/`gpu` capability) rather than at a busy
queue. That is not confirmed from here (the build cannot read the live DB or the runner config);
the next 07:00 note will name the runners and what they offer.

**Decision.**

- **One note a night.** `unstarted_noted_at` now holds when the latest note went. The note is due
  once the latest 07:00 on the loop's clock (`pipeline.LastAt`) is past the run's first 07:00 and
  after the last note; a run that carries over gets a note on every night it misses. Each note
  posted is a `regression_missed` event (data: the run, the night's 07:00, the why), so missed
  nights can be counted.
- **Runners record their kinds.** The claim writes the kinds of work it asks for to the runner's new
  `kinds` column (migration 0038; empty: items only, as the claim reads it).
- **The note names the cause.** When runners were seen in the last day and none of them asks for
  regression runs with every resource the suite needs (`pipeline.NoRegressionTaker`), the note
  starts with "no runner takes it: it needs one that claims regression runs and has emulator, gpu,
  voice (seen: <runner> has … and takes …)" and drops the misleading "it starts after 10m of quiet".
  What keeps the queue busy, another open run and claude's pace are still added.

**Left to the user.** A carried-over run still starts in the daytime once the queue has been quiet
for 10 minutes (as before). Whether it should instead move to the next night's set time once its
night is missed is a product call this change does not make.

**After it lands.** Check the dev box runner's config: `stages` includes `regression`, and
`capabilities` include emulator, gpu and voice. The next 07:00 note says which one is missing.
