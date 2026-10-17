# Agent sandbox: the runner runs the harness for build and review agents

2026-10-16 · item #66. Build and review agents run in Claude Code's sandbox, which cannot read the
harness's credentials or `/dev/kvm`, and in which every command gets a network namespace of its
own: a jarvisd an agent starts dies when its command ends (#46, #56, #64), and only the runner's
verify step ran the harness. Each choice below is enforced by a test in
`internal/runner/agentsandbox_test.go` or by `scenarios/52-agent-sandbox.json`.

- **Opt-in per repo** (`runner.json`): `"agent_sandbox": {"up", "down", "env_file", "requests",
  "secret_env_files", "up_timeout"}`. `up` and `env_file` are required; `env_file` is
  worktree-relative; `{sandbox}` in any of them is the item's sandbox name (`loop-<id>`), as in
  `cleanup`. Their environment has `LOOP_ITEM`, `LOOP_SANDBOX`, `LOOP_WORKTREE`, `LOOP_REPO`,
  `LOOP_BASE`, `LOOP_MIRROR` and `LOOP_STAGE`.
- **Lifetime: one agent attempt.** For build and review, after setup and before the agent, the
  runner runs `up` under the fence (the same bubblewrap as verify: the harness's env files
  readable, credential stores masked). The fence's pid namespace ends with its first process, so
  `up` runs as `(up) || exit; touch ready; exec sleep infinity` in **one fenced process the runner
  holds** for the attempt: whatever `up` backgrounds keeps running. The runner waits for the ready
  marker (heartbeating; `up_timeout`, default 10 minutes), runs the agent, then runs `down` (fenced)
  and kills the held process's group, which takes the namespace and everything in it. The same
  happens in `cleanup()` on any failure or stop. Output goes to `agent-sandbox.log` in the run dir.
  The refused-result retry brings it up again.
- **Down before verify.** Verify uses the same `LOOP_SANDBOX` name and brings up its own sandbox
  from the runner's config, so the agent's is torn down first (it is down before the result is
  even checked). Nothing the agent's sandbox did is trusted by verify.
- **No verify lock for `up`.** Holding the machine's verify lock for a whole agent run (up to 45
  minutes) would stall every other verify. A sandbox that uses the shared model (jarvis-server's
  `--llm shared` through its tap) shares it as the user's own sandboxes do. Requests (below) take
  the lock.
- **Trusted files.** `up`, `down` and requests run files from the worktree outside the agent's
  sandbox, so the runner refuses to run them while the worktree changes (committed, staged,
  unstaged or untracked) any of the repo's `deny_write` paths against the base, as verify does.
  For jarvis-server, `scripts/sandbox` and `scripts/mobile-shot` are on that list.
- **The env file** is what reaches the agent: local URLs and sandbox-local credentials. It must be
  a regular file (no link) of at most 64 KB, and the runner refuses it (the run fails before the
  agent starts, with the log's tail and without the value) when it contains the value of any
  `*_KEY`, `*_TOKEN`, `*_SECRET` or `*PASSWORD*` variable of at least 8 characters from
  `secret_env_files` (default: every `*.env` in the harness's config dir). The same values are
  redacted from request logs and from the failure message. The agent's own settings still deny
  reading the harness's config dir. The env file's directory must be ignored by git (jarvis-server's
  `.sandbox/` is), or the result is refused as uncommitted.
- **Loopback.** The run gets its own settings file, `settings/<repo>-<stage>-loop-<id>.json`, with
  `127.0.0.1:<port>` and `localhost:<port>` added to `allowedDomains` for each loopback port the env
  file names (loopd's port never; loopd's addresses stay in `deniedDomains`). This costs the
  cross-item prompt cache for opted-in repos' runs (their argv names a per-item file); a run still
  caches within itself. **Not verified:** each sandboxed command has its own network namespace
  (probed on 2026-10-16: a server in one command is not reachable from the next), so a host port is
  reachable only through Claude Code's proxy, and only for clients that send loopback through a
  proxy (`curl --noproxy '' -x "$HTTP_PROXY"`); Go's `net/http` never proxies loopback. The brief
  says so. Whether the proxy forwards to host loopback ports was not testable from inside an agent
  (the classifier refused the probe); check it on the first real run.
- **Requests** (the reliable path, and the only one for KVM): the agent writes
  `.loop/requests/<id>.json` = `{"run": "<name>", "args": {...}}`; the runner polls the dir while
  the agent runs, runs the named command from `requests` (never anything from the agent), fenced,
  under the verify lock (the emulator and GPU are single), with `LOOP_HEAD` (the worktree's HEAD:
  commit first), `LOOP_EVIDENCE_DIR` (in the run dir) and each arg as `LOOP_REQ_<NAME>` (names
  `[a-z][a-z0-9_]*`, one-line values of at most 500 characters; never spliced into the command
  text). It copies the plain files the command wrote into `.loop/evidence/<id>-<file>` and answers
  in `.loop/requests/<id>.done.json` (`exit_code`, `error`, the log's last 40 lines redacted,
  `files`). At most 10 per attempt, 20 minutes each. Requests run in the item's own checkout, so
  a jarvis-node-mobile item (whose shots run from a jarvis-server checkout in verify) gets no
  agent sandbox from this; that would need a request with a `repo`, as verify steps have.
- **The brief** (build and review, "This run") names the sandbox, the env file, the proxy caveat
  and the requests; the build and review skills say to use it instead of starting their own.

## For jarvis-server (the user's runner.json; not in this PR)

```json
"agent_sandbox": {
  "up": "JARVIS_HARNESS_ENV=$HOME/.config/jarvis-harness/prod-background.env scripts/sandbox up --name {sandbox}",
  "down": "scripts/sandbox down --name {sandbox} --purge",
  "env_file": ".sandbox/{sandbox}/sandbox.env",
  "requests": {
    "mobile-shot": "JARVIS_MOBILE_REPO=$LOOP_MIRROR scripts/mobile-shot --sandbox {sandbox} --ref $LOOP_HEAD --screens \"$LOOP_REQ_SCREENS\" && d=$(ls -d .sandbox/{sandbox}/mobile/*/ | tail -1) && cp \"$d\"index.json \"$d\"*.png \"$LOOP_EVIDENCE_DIR/\""
  }
}
```

`JARVIS_HARNESS_ENV` and the `mobile-shot` flags are as jarvis-server's scripts document them
today; check them against its `scripts/sandbox` and `scripts/mobile-shot` before adding this. Its
`sandbox.env` holds the dev users' password and a sandbox superuser JWT (sandbox-local), not the
installed app's key.
