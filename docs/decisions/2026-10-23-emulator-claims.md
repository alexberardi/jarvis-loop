# What an emulator claim gets

2026-10-23 · item #546, after #523 and #542 both hit the same wall.

`resources` only gated **who claims** an item; it never changed **what the claim got**. Triage set
`resources=[emulator]` on #542, the devbox offers the capability and loopd's limits allow one, and
the build's sandbox still had no `/dev/kvm`, no `adb`, `emulator` or `maestro` on PATH, and no
jarvis-server checkout to run `scripts/e2e mobile` from. So an emulator-tagged item could not use
an emulator anywhere, and two runs were spent discovering that.

## The ask's two premises that do not hold

- **"Make the fence pass `/dev/kvm` through."** The fence already does: `fence_bwrap.go` binds the
  host root with `--dev-bind / /`, so fenced commands get the real devices, which is why the
  regression stage runs `scripts/e2e mobile` on the devbox's emulator in exactly this fence. What
  has no `/dev/kvm` is **the agent's Claude Code sandbox**, and its settings have no device knob at
  all (`GenerateSettings`: filesystem, network, `excludedCommands`). Nothing in this repo can pass
  a device into it, so that part of the ask is not implementable as written; the agent reaches the
  emulator only through something the runner runs for it.
- **"Put the Android tooling on PATH."** `adb` is denied twice on purpose — in the agent's
  settings and in `loop-guard` ("emulator work goes through the harness scripts, which pass the
  serial") — because the host's adb server also holds the user's own emulator. Putting it on PATH
  without lifting both denials changes nothing, and lifting them reverses
  `2026-10-15-runner-choices.md`. Not done here, and not as a side effect of anything.
  In the fence there is nothing to do either: a fenced command gets the runner service's own
  environment (`agentEnv` → `scrubEnv(os.Environ())`), the same one the regression stage's
  `scripts/e2e mobile` runs with when it drives the devbox emulator (#526's run). If regression can
  find the SDK and Maestro, so can `loop-harness run`.

This is the shape `2026-10-16-harness-broker.md` already chose for the same wall: the agent
sandbox stays as it is, and the runner runs harness commands for the agent in the fence its verify
steps use. The real gap was narrower: **the brokered path could not reach another repo's scripts.**

## Decided

- **A repo's harness may live in another repo.** `"harness": {"repo": "jarvis-server", "up": …,
  "down": …}`: the runner checks that repo out fresh at its base under
  `<work_root>/<item>-harness-<key>` before the agent starts, and `up`, `down` and every
  `loop-harness run` of the session run **there** instead of the agent's worktree, with the
  worktree writable beside it and its path in `$LOOP_WORKTREE` (as the regression run passes
  `--mobile-repo`). `$LOOP_HARNESS_DIR` is the checkout. It goes with the session, after `down`.
  This is route A of #546's triage: a jarvis-node-mobile agent can now run jarvis-server's
  `scripts/e2e mobile --mobile-repo $LOOP_WORKTREE` itself. The checkout and its removal are
  `VerifyStep.Repo`'s (`verify.go`); the writable second checkout is the regression run's.
  It supersedes the broker decision's "commands always run in the session's worktree" — the client
  still never names a directory, the config does.
- **The guard and the fence do not loosen.** Every `loop-harness run` still passes the run's own
  guard config first (now with the checkout as its cwd, which is where the command runs: the
  worktree's protected files stay protected through `$LOOP_WORKTREE/...`), and the fence still
  masks the user's credentials and gives no git credentials. `adb` stays denied in both places, so
  the route is the repo's harness scripts, which pass the serial themselves.
- **The purge runs where the scripts are, or says it cannot.** Before each verify step the runner
  takes down the agent's and verify's sandbox homes with the harness's `down` (#327), and it ran
  that fenced in the agent's worktree. With `harness.repo` the scripts are not there, so the
  command exited non-zero and the miss was logged as `harness down failed (nothing to take down?)`
  — which reads like success — and the "purge and retry once" repeated the step with nothing
  purged. Now the down runs in the directory that holds the scripts: the worktree for a
  worktree-scoped harness, and for a repo-scoped one the step's **own** checkout, which exists when
  the step's `repo` is the harness's. For any other step there is no copy to run, so the purge is
  skipped with a warning naming the `repo:` to add, and an environment failure is not retried with
  nothing purged. (`canPurge`/`harnessDownDir` in `verify.go`.)
- **A runner-side verify step stays the gate.** `VerifyStep.Repo` already runs
  `scripts/e2e`/`mobile-shot` from a jarvis-server checkout outside the agent's sandbox, after the
  build (route B, the worked example in `2026-10-15-all-repos.md`). It needs no code: it is the
  user's `runner.json`, which no agent can read or write. A and B are complements — A lets the
  agent iterate, B proves the result before the push — and either one counts as a route below.
  A step with `when_changed` globs counts as well, although what the agent commits decides whether
  it runs: the runner cannot know the diff before the agent makes it, and refusing such a config
  would refuse the natural one (`when_changed: ["app/**"]` for the mobile app). Instead the brief
  names a conditional step with its globs and says it may be skipped, so an agent whose change
  lands outside them reports an unrun flow in `not_verified` rather than assuming the runner ran
  it.
- **Every brief says what the item needs.** The "This run" section now names the item's
  `resources`, and an `emulator` item's brief says plainly that the agent's own sandbox cannot
  reach one (no `/dev/kvm`, no tooling, `adb` refused) and which route this run has: the harness
  through the runner, the runner's verify steps after the build, or none. It is in the per-item
  part, so the cacheable prefix (`2026-10-15-prompt-caching.md`) is untouched.
- **A build with no route fails loudly.** A build of an item that needs `emulator` on a repo with
  neither a harness for its agents nor a verify step ends before the agent starts, with a runner
  error naming what to add to `runner.json`; the item goes to the user. Better the user adds one
  line than an agent spends a run finding out, as #523's and #542's did.
- **A review run is told the route is the build's, not that there is none.** The brief's list of
  verify steps is built for the build stage only, so a review of an emulator item on a repo that
  does have steps would have been told "this repo has no harness through the runner and no verify
  step" — true of the run, misleading about the repo. A review with no harness now reads that a
  verify step runs after the build rather than after it, and to judge the build's evidence of the
  emulator run instead of repeating it.
- **Triage keeps no route, and is told so.** The harness through the runner stays build and review
  only (`hasHarness`): a triage run has no repo worktree to run a harness in, and widening it
  would start a sandbox for every triage of a harness repo. An emulator item's triage brief says
  instead that it cannot reproduce on the emulator and should name the commands a build must run.
  #546's own triage hit this and said so in its notes, which is the behaviour we want.

## Not decided here

- Whether jarvis-node-mobile's live `runner.json` gets `harness.repo`, a verify step with
  `repo: jarvis-server`, or both: that file is the user's and no agent can read it. The config
  error and the brief now say what is missing when it is missing.
- The config is not checked against the capability: a repo cannot declare that its items need an
  emulator, only an item can, so the check belongs to the run (above), not to `LoadConfig`.
- Whether `up` for a repo-scoped harness should hold the verify lock, and GPU/emulator contention
  between an agent's harness and another item's verify: unchanged from the broker decision.
- Anything measured on a real emulator. The build of this item ran in an agent sandbox with no
  `/dev/kvm` and no Android tooling, so the four Maestro flows of #542 are still unrun; that is
  the proof to take once this is merged.
