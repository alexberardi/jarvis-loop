# Proof commands run the repo's setup; an environment failure never reopens

2026-10-19 · item #282. #263's proof (`go test … ./internal/modules/cc/` in jarvis-server) failed
twice in 0s with `[setup failed]: pattern libs/linux-amd64: no matching files found`, and loopd
reopened it as #281. The proof's checkout of main never ran the repo's setup
(`scripts/fetch-sherpa-libs.sh`), so the package could not even build its embed; after the fetch all
its tests pass. Extends [2026-10-18-qa-proofs.md](2026-10-18-qa-proofs.md). Enforced by
`TestProofRunsTheRepoSetup`, `TestProofEnvironmentErrorDoesNotReopen` (`internal/runner/proof_test.go`)
and `TestApplyProofChecked` (`internal/pipeline/proof_test.go`).

- **Before a command check, the runner runs the repo's `prepare` and `setup` commands** in the
  proof's checkout, fenced, as before a build or a regression run, logged in
  `<work_root>/proofs/<item>-<check>/setup.log`. Nothing new is cached: like build, verify and
  regression, each fresh checkout runs them again under the same fence caches. Each command check
  has its own checkout, so it runs them once per check.
- **An environment failure is a third result, `error` (`pipeline.CheckError`)**, not `failed`.
  It is a result only, never a check's state: loopd records it as the check's evidence ("could not
  run: …") and its check time, and leaves the check's state and fail count as they were, so it
  never counts toward the two-failures reopen and a failure before it still needs one more real
  failure. The runner logs a warning for each. Chosen over leaving the check out of the report
  (as for a missing fence) so the user sees on the item why its proof is not getting anywhere.
- **What counts as an environment failure** (`envFailure`, kept narrow): a setup or prepare command
  failing; a command exiting 127 (a tool not installed); output with go's `[setup failed]` (a
  package whose embedded files or deps are missing, which the merged change does not cause once
  setup ran). **`[build failed]` stays a real failure**: a compile error the merged change brought
  shows the same way, and the proof is there to catch it. A timeout stays a failure too.
- An item whose environment keeps failing stays in qa with the error on its check; fixing the
  repo's setup (the runner config) is the user's.
