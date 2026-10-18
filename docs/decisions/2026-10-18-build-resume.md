# Interrupted Claude builds resume their session (2026-10-18)

Item #200. A Claude build cut off by a stop, the time cap, the runner shutting down or a crash
started over on its next claim: on 2026-10-17 a runner restart threw away ~45 agent-minutes. The
work itself carried over (WIP pushed on the branch), the session did not.

## Decision

- **The runner keeps the session, not loopd** (`internal/runner/buildsession.go`): when a Claude
  build's stream gives its `session_id`, the runner writes `WorkRoot/sessions/<item>-build.json`
  (session, stage, the branch head the run started from). A crash reports nothing to loopd, so only
  a local record survives it; and a session lives in `~/.claude/projects/` of the machine that ran
  it, so another runner (a Mac, after the dev box) has no record and starts a new session, which is
  all it could do.
- **Interrupted** (stopped, shutdown, time cap): after the WIP commit and push, the record gets the
  reason, the time and the new head. **Any verdict** (pr_opened, question, failed, usage limit, a
  refused result) drops it. A record still "running" on the next claim is a **crash**: the runner
  first commits and pushes what the old worktree still holds as `WIP: … (crashed)`, then resumes.
- **The next claim resumes** when the record is the same stage, the branch's head is still the one
  the session left (nobody else moved it), and the session was resumed fewer than
  `MaxBuildResumes` (2) times, so a build that keeps hitting the time cap starts afresh in the end.
  The worktree is made at the same path (`WorkRoot/<id>-build`), which is where claude looks for
  the session. The prompt is a short note ("interrupted at <time> by <reason>; continue; check
  `git status` and `git log` first"), not the brief: the session has it, and `.loop/brief.md` is
  still written for the agent to re-read.
- **Fallback:** a resume that claude refuses ("No conversation found", as plan turns detect it)
  runs a new session with the whole brief in the same claim, and the record is dropped. A refused
  result's retry is a new session too, as before.
- **Counting:** a Claude build's run fields carry `resumed` (`true`/`false`) and `resumed_from`;
  the flow panel (#201) reads them. Qwen builds keep their own resume (loopd's `BuildSession`).

The stage-prefix check (`same_prefix`) accepts a resumed build: its prompt is the note and its
argv adds `--resume <session>`, as plan turns' do.
