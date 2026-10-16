# The runner attaches the failing CI log

2026-10-16 · item #28. **Seen:** on #25, main went red at 7e32b29. Triage could not see the failing
run (agents hold no GitHub credentials, by design), could not reproduce a timing flake locally, and
asked the user twice; the user could not help either. The runner holds `gh`, so it fetches the log
itself. What was chosen, each covered by a test or a scenario:

- **What is fetched.** `gh run view <id> --repo <slug> --log-failed` (`GH.FailedLog`, new in
  `LandGitHub`), for at most 5 failed runs. Run ids come from the failed checks' URLs
  (`…/actions/runs/<id>/job/<j>`, one fetch per run) on a PR, and from `gh run list`'s `databaseId`
  (now asked for) on main.
- **What is kept** (`ExtractCIFailure`, pure, `cilog_test.go` on fixtures in `testdata/gh/`): gh's
  `job<TAB>step<TAB>timestamp` prefix dropped and the lines grouped by job, ANSI stripped, 100 lines
  before and after each Go `--- FAIL:` line, loop-sandbox `Scenario … (FAIL` or `FAIL <name> 1.2s`
  line (overlapping windows merged), else the job's last 200 lines; capped at 256 KB. The failing
  test names go at the top. Token-like strings are redacted: GitHub, Slack and `sk-` tokens,
  `Bearer`/`Basic` credentials, `…token=`/`password:`-style values, 64+ hex digits and 32+
  base64-like characters mixing cases and digits (a 40-digit commit sha stays).
- **Where it goes:** evidence `ci-failure.log`, label `CI failure on <sha7>: <check or job names>`.
  loopd only takes evidence from a runner holding the item's live lease, and that rule stays
  (it is access control):
  - **Red CI on a PR:** land holds the lease, so it uploads the log before it reports
    `back_to_build` (`TestLandAttachesTheCILog`, scenario `ci-red-unchanged`).
  - **Main red:** the P1 is a new item the runner holds no lease on, and the watch runs after the
    land lease ended. So the runner reads the merge commit back from the P1's ask (`MainRedAsk` /
    `MainRedCommit`: only a `finding` titled "main red after #…" for the item's own repo) when it
    **claims the P1 for triage**, lists main's runs on that commit again and uploads the log under
    triage's lease (`TestTriageOfMainRedAttachesTheCILog`, scenario `main-red-ci-log`). A later
    triage of the same item reuses the attached file.
- **Briefs:** triage gets it as `.loop/context/ci-failure.log`, build finds it in `.loop/triage/`
  (the newest one), and both briefs say so, with: a test that passes locally but fails in CI is
  usually a race on a slower machine; look for the race, don't ask the user.
- **Never blocking:** a log that cannot be fetched or uploaded is logged (land) or listed under
  "Not available this run" (triage); the item moves on either way (`TestLandCILogFailureDoesNotBlock`).
- **Side effect:** the log uploaded under a triage lease counts as evidence under that lease, which
  is what loopd's "reproduced needs evidence" rule counts. For a main-red P1 the failing log is
  real evidence of the failure, so this is accepted.
