# Pipeline verify: the runner judges regressions, re-runs the suspects, skips by path (2026-10-21)

Item #392. jarvis-server's verify ran the pipeline scenarios (the real qwen3.5-9b model) from a bash
line in runner.json, with the rule "a regression passes 3/3 on main and 0/3 on the change"
(jarvis-server docs/dev/e2e.md). About 11 scenarios fail on every run, ~9 of them on main too, and
on most runs one other scenario splits 3/3 vs 0/3 by chance. So #376 (README.md and docs/STATUS.md
only) failed twice, #388 (scripts only) once, and #298, #288, #265 and #338 before them; each cost
the user an escalation and a re-run.

## Decision

- **The rule moves into the runner**, a verify step with `pipeline` set (`VerifyStep.Pipeline`,
  `internal/runner/pipelineverify.go`). The runner runs it in rounds, each one run of the step's
  command (default `PipelineCommand`: `scripts/sandbox up`, `go run ./tools/pipeline run --json
  [--repeat N] [--filter RE]`, `down --purge`), with `LOOP_PIPELINE_SIDE` (head or base),
  `LOOP_PIPELINE_REPEAT`, `LOOP_PIPELINE_FILTER` (anchored, `^(a|b)$`), `LOOP_PIPELINE_SANDBOX`
  (verify's own name) and `LOOP_PIPELINE_OUT` (where the JSON summary goes) set:
  1. the change, every scenario, `repeat` runs each (default 3); a **candidate** failed every run;
  2. the base, the candidates only, `repeat` runs: a **suspect** passed every run there; the rest
     fail on main too (or are new) and are not the change's;
  3. the change, the suspects only, `confirm` runs each (default 5): one that passes once is
     **flaky**;
  4. the base, the rest, `confirm` runs: one that passes every run is a **regression**; one that
     fails once is flaky.

  The step fails only on regressions (status `failed (regressed: who am I; 12m)`); flaky ones are
  named in a passing status (`passed (9m; flaky, not counted: who am I)`), which is the PR footer's
  line. The log (the next build's `verify.md` on a failure) lists each round and verdict.
- **Why these numbers.** A real regression (always fails on the change, passes on main) confirms at
  every count. A scenario that passes half the time on both sides gets through 3+3 as a suspect
  1 time in 64, and also through 5+5 1 time in 1024, so with ~10 such scenarios a false regression
  goes from most PRs to about one in 6,500. A regression that only sometimes fails on the change is
  missed, as it was by the 3/3-vs-0/3 rule before.
- **Cost.** A clean change costs one round (as before); a change with candidates adds the base
  round for those scenarios only, and the confirm rounds only for suspects. The base is a fresh
  detached checkout of the mirror's base branch (`<work>/<item>-verify-base`, the repo's `prepare`
  commands run in it, removed afterwards). The step's `timeout` covers every round: set it to what
  the four rounds can take (e.g. `90m`), inside loopd's per-run cap.
- **Skip by path.** A change that cannot reach the model's behaviour does not run the step:
  `when_changed` (already there) lists what can. The skip shows as `skipped (no changed path
  matches …)` in the footer.
- Not done: idea 3 of the ask (count a regression only when the scenario's area matches the changed
  paths) needs a scenario-to-paths map in jarvis-server; with the confirm rounds and the path skip
  it should not be needed.

## The user's part

The runner reads its config only from runner.json, which no agent may edit. After this merges and
the runner runs it, replace jarvis-server's pipeline verify entry with:

```json
{"name": "pipeline scenarios", "pipeline": {"repeat": 3, "confirm": 5}, "timeout": "90m",
 "when_changed": ["**/*.go", "go.mod", "go.sum", "tools/pipeline/**"]}
```

and add to `when_changed` wherever jarvis-server keeps prompts and tool definitions that are not
`.go` files (embedded `.md`, `.txt` or `.json`), so a prompt change still runs it. With no `run`
the runner uses `PipelineCommand`; a `run` replaces it (it must write the summary to
`$LOOP_PIPELINE_OUT`, honour the filter and repeat, and exit 3 when its sandbox did not come up).
