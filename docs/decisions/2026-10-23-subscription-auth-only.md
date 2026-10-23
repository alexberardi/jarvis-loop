# The loop's agents run on the subscription only (2026-10-23)

Item #548, phase 9 §10. Subscription auth was true by omission: nothing set `ANTHROPIC_API_KEY`,
so the OAuth login was used. But `ScrubEnv` (`internal/runner/runner.go`) dropped only `LOOP_TOKEN`,
`LOOP_URL` and `SLACK_*`, so a stray `ANTHROPIC_API_KEY` in the runner's own environment — exported
in a shell profile, inherited from a parent process, set for an unrelated tool — would have been
handed to every agent run and silently billed every build to that key. The runner also never read
the `apiKeySource` the `system/init` event already carries, next to the `permissionMode` it does
check.

## Decision

Two absolute rules, neither configurable:

1. **No Anthropic variable reaches anything the runner starts.** `ScrubEnv` drops every
   `ANTHROPIC_*` variable (key, token, base URL, model, headers — the prefix, not a list), plus
   `CLAUDE_CODE_USE_BEDROCK` and `CLAUDE_CODE_USE_VERTEX`, which redirect billing the same way.
   The runner needs none of them, so there is no exception to keep in step with the CLI's variable
   list. `ScrubEnv` is the one chokepoint: the agent's own environment (`agentEnv`), the helper git
   and `gh` commands, and the fenced setup/verify/proof/regression/harness commands (`fencedEnv`
   on top of it) all come through it. It is exported so the loop's own harness
   (`internal/sandbox`), which builds the fake claude's environment itself, applies the same rule
   instead of a second list of names.
2. **A session that is not on the subscription dies.** The init event's `apiKeySource` is recorded
   on every advance (`api_key_source`) and asserted against `wantAPIKeySource` = `"none"`: anything
   else ends the run as `auth_mismatch`, which is handled exactly where `mode_mismatch` is (item
   runs, plan turns, orchestrator turns) — the item goes to needs-you naming the source, and the
   runner stops claiming until it is restarted. Pausing is the point: a key in the runner's
   environment outlives the run, so every later claim would burn it too.

`--bare` stays unused: it skips hooks (the guard would never run) and never reads the OAuth login,
so auth would be strictly an API key. `internal/runner/settings_test.go` already fails if it ever
appears in the argv.

## Why `"none"` is the expected value

`apiKeySource` was the open question in `2026-10-15-claude-headless-facts.md` ("the exact value for
subscription auth"). The runner's own saved streams answer it: across all 3334 real runs it has kept
(`<work_root>/runs/*/stream.jsonl`, CLI 2.1.287, the loop's whole history to 2026-10-23, every
stage, Opus and Sonnet) the init event says `"apiKeySource":"none"` every time and nothing else —
claude reports "none" when it used the OAuth login and no API key at all. The dangerous values are
the ones that name a source (`ANTHROPIC_API_KEY`, `apiKeyHelper`, `temporary`, …).

An empty or missing reading also fails the assertion. That is deliberate: "the CLI stopped telling
us" is not evidence that the run is on the subscription, and the failure is loud (one item in
needs-you, claims paused) rather than silent. A CLI upgrade that renames the field will be noticed
on the first run, which is the behaviour the `permissionMode` check already has. That case gets
its own needs-you advice (`mismatchFix`): an empty reading is the one auth mismatch an `ANTHROPIC_*`
variable may not have caused, so it says to check the environment *and* whether the CLI still
reports the field, which would need a code change here rather than an environment one.

## Not configurable

The expected value is a constant, not a config field, on purpose: a field in
`internal/runner/config.go` would be a way to turn the rule off, and that file is on the `tokens`
hold rule (`2026-10-18-narrow-loop-holds.md`) precisely because auth settings should not move
without the user.
