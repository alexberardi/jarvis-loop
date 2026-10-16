# QA after deploy: ask for the loop's own check once a build with it runs

2026-10-16 · item #26. Seen on #22: right after a jarvis-loop change marked for qa merged, the bot
asked the user to "Press Verified when it works", but a loop change runs only once the main session
deploys new binaries, so there was nothing to check yet. Extends the QA lane
([2026-10-15-qa.md](2026-10-15-qa.md)); each point is enforced by a test or scenario
`scenarios/47-qa-after-deploy.json`.

## What was chosen

- **Config: `qa_after_deploy: true`** on the repo in the runner's config (`RepoConfig.QAAfterDeploy`,
  next to `qa`, for the same reason `qa` lives there). It implies `qa`. Set it for
  `alexberardi/jarvis-loop` (the sandbox's stand-in has it too). Other repos, the mobile app's
  `qa: true` included, keep today's behaviour (scenario 33).
- **Merge:** for such a repo the land script sends `qa_after_deploy: true` (and `needs_qa: true`)
  with `merged`. loopd moves the item to qa as before and sets **`awaiting_deploy`** on it (a column,
  migration `0010_qa_deploy.sql`; the merged event's data carries `awaiting_deploy: true`).
- **Slack at merge:** "<@approver> #N: Merged; I'll ask for your check once it's deployed." with the
  PR link and merge commit, and **no buttons**. A reply in the thread is not taken and says so.
  `/loop verified` and `/loop reopen` still work while it waits, and end the wait, so nothing gets
  stuck if no deploy is ever reported.
- **Deployed, reported by the runner.** The runner reads the commit it was built from
  (`runner.BuildRevision`: Go's `vcs.revision` build stamp, via `debug.ReadBuildInfo`). On every
  poll it lists the qa items awaiting a deploy (`GET /items?awaiting_deploy=1`). For each one of a
  repo it has with `qa_after_deploy`, it checks in that repo's mirror whether its build contains the
  merge: `git merge-base --is-ancestor <merge_sha> <revision>`. The mirror is fetched only when it
  lacks one of the two commits. If the build contains it, it records **`deployed`** (`POST
  /items/{id}/events`, kind `deployed`, data `{revision}`, runner auth, no lease needed). loopd
  clears `awaiting_deploy`. The bot then posts the usual ask with **Verified** / **Reopen**:
  "Deployed — waiting for your check. <PR> (merged as `abc1234`) is running in the build
  `def5678`." A second report (another runner, or the user already checked) is `wrong_stage` and is
  not recorded as a refusal.

## Why this design

- **Ancestry, not equality or time.** A build made from a later commit contains the merge too, and
  merges can land out of order, so comparing timestamps would be wrong. Ancestry is the correct test.
- **In the runner, not loopd.** loopd has no git and no mirrors; the runner already has the mirror
  and fetches it. loopd only records what the runner saw.
- **The runner's build stands for the loop's.** loopd and the runner are built from the same
  commit and deployed together, so one revision is enough. If only loopd were redeployed, the ask
  waits until the runner restarts on the new build (or the user presses `/loop verified`).
- **Every poll, not only at start.** A running binary's revision never changes, so start-only
  would be correct in principle. But a start-time check could miss an item if loopd was down at
  that moment, or if the merge reached loopd after the runner started. The per-poll check costs one
  list request; it runs git only for items awaiting a deploy, and fetches only for a missing commit.

## Limits

- **No stamp, no report.** A binary built with `go run`, `go install pkg@version`, outside a
  checkout, or from a checkout with uncommitted changes (`vcs.modified=true`) has no usable revision.
  The runner logs this at start and reports nothing; use `/loop verified`. Deploy with a plain
  `go build ./cmd/...` in a clean checkout of the commit being deployed.
- **A revision the remote does not have** (a local commit that was never pushed) cannot be checked.
  The runner logs it and reports nothing.
- `loop-runner --once` does not report deploys.
- The digest still lists an item awaiting a deploy under "Waiting for your check", without saying
  that it waits for a deploy.
