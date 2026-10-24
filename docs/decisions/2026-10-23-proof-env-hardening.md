# A proof waits for the merge on main, builds a python venv, and knows a broken toolchain

2026-10-23 · item #532. Five spurious reopens in ~26h, each closed not reproducible: #504 (the
proof ran a minute after the merge against a view of main without it), #515, #527 (`python -m
pytest` collected nothing on the devbox — 8 import errors in 0.05s — right after a merge the review
had green at the same head), #510 (`mise exec go@1.27 -- go test -c` toolchain trouble), and the
mbp's `go: no such tool "vet"` reopens of #292/#443/#444. All three causes are the runner's
environment, not the change's. Extends [2026-10-19-proof-setup.md](2026-10-19-proof-setup.md) and
[2026-10-22-proof-env.md](2026-10-22-proof-env.md). Enforced by `TestProofWaitsForTheMergeSHA`,
`TestProofBuildsAVenvForAPythonCommand` and `TestEnvFailure` (`internal/runner/proof_test.go`) and
the `proof-toolchain-error` scenario.

- **A command check runs only once the fetched main has the item's merge commit** (`git merge-base
  --is-ancestor` in the mirror after the fetch, as deploy's `buildContains`). Until then the check
  is not run and not reported — it stays pending, exactly as for a missing fence, and the next
  pass (`ProofEvery`) retries. Chosen over reporting an error so a short replication lag leaves no
  noise on the item.
- **The runner builds a `.venv` for a python command when the repo's setup made none**: when the
  command names `python`/`pytest`/`pip`, the checkout has no `.venv/bin` after setup, and the repo
  has something to install (`pyproject.toml`, editable with its `dev` extra, else
  `requirements*.txt`). Appended to the check's `setup.log`; a failure is a setup failure
  (`CheckError`, never a strike). This reverses 2026-10-22's "the runner does not build venvs
  itself": proof commands written by the orchestrator kept assuming an environment the device
  repos' setups do not make, and each miss cost a reopen.
- **`envFailure` knows a broken toolchain and a collection failure**: besides exit 127 and
  `[setup failed]`, now `no such tool`, `cannot find GOROOT`, `mise ERROR`, pytest's `ImportError
  while importing test module` and `error(s) during collection`, and an errors-only pytest summary
  (`N errors in …`, nothing passed or failed) from a run under one second. All are `CheckError`:
  recorded on the item, never counted toward the reopen. **`[build failed]` still strikes**, and so
  does any fast failure without one of these signatures: the merged change may be exactly what the
  command tests for (2026-10-19 stands).
