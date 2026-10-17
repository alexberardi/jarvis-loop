# Harness broker: agents use the harness through the runner

2026-10-16 · item #67. Build and review agents run in Claude Code's OS sandbox: a strict network
allowlist (no host loopback), no read of `~/.config/jarvis-harness`, no `/dev/kvm`, and what a
command starts dies when it ends. So they could not run jarvis-server's harness, and every PR listed
the real run as not verified (#46, #56, #64). The agent sandbox stays as it is, and
`excludedCommands` stays empty (`2026-10-15-runner-choices.md`). Instead the runner runs harness
commands for the agent, in the fence its verify steps already use (`fence.go`).

## How it works

- **Config.** A repo's `runner.json` entry may have
  `"harness": {"up": "<bash>", "down": "<bash>", "timeout": "20m"}` (timeout per command, default
  20m). `loop_harness` (default: next to `loop-runner`) is the client binary. jarvis-server:
  `up` = `grep -q INSTALLED_LLM_LABEL scripts/sandbox && export JARVIS_HARNESS_ENV="$HOME/.config/jarvis-harness/prod-background.env"; scripts/fetch-sherpa-libs.sh >/dev/null && scripts/sandbox up --name $LOOP_SANDBOX`,
  `down` = `scripts/sandbox down --name $LOOP_SANDBOX --purge`.
- **Socket and session.** The runner listens on `<work_root>/harness.sock` (0600; a stale one is
  removed), started by the first session. For each agent run of a build or review of such a repo it
  opens a session with a random 32-byte token and writes `.loop/harness.json` `{socket, token}` in
  the worktree (`.loop/` is never committed). The session ends as soon as the agent does (exit,
  time cap, lease lost, runner shutdown), before the runner's own verify, which starts its own
  harness under the same sandbox name. `loop-runner` also ends any left when it stops.
- **Who may call.** Tokens are compared in constant time. The token sits in a file other items'
  agents could read, so the broker also checks the caller with `SO_PEERCRED`: the runner's uid,
  and a working directory inside the session's worktree. Commands always run in the session's
  worktree; the client never names a directory.
- **Protocol.** One JSON line `{token, op, cmd}`; replies are JSON lines, output frames then
  `{exit}` or `{error}`. Ops: `up`, `run`, `down`, `status`. The client exits with the command's
  code (125 when the broker refused, 124 on a timeout). Every command's full output goes to
  `.loop/scratch/harness/<n>.log`. Requests run one at a time per session; the client going away
  (the agent's Bash timeout) cancels its command.
- **Environment.** As verify's: the agent's environment through `fencedEnv` (no tokens, keys or
  secrets), plus `LOOP_ITEM`, `LOOP_WORKTREE`, `LOOP_EVIDENCE_DIR`, `LOOP_SANDBOX`, `LOOP_REPO`,
  `LOOP_BASE`, `LOOP_MIRROR`. What `up` exports (the harness's env file) stays in up's shell: later
  commands get the runner's environment, not the keeper shell's.
- **Agent side.** Only for harness repos: `sandbox.network.allowUnixSockets` = the socket (never
  `allowAllUnixSockets`); `<work_root>/bin` (holding only a `loop-harness` link) first on PATH;
  strict mode allows `Bash(loop-harness *)`; the brief's "This run" part says what it can run and
  that verify still runs after (the static part is unchanged, so the prompt cache is too).

## Pid namespaces: commands run inside the keeper

Every fence has its own pid namespace (`--unshare-pid`) and a private `/tmp`. jarvis-server's
`scripts/sandbox` keeps jarvisd's and the tap's pids in pidfiles (`running()` is `kill -0`), used
by `down`, `status`, `llm` and `list`. In a new fence those pids mean nothing, so the ask's "run =
a new fence" would break a mid-run `down` and `status`. Chosen (the triage's option 1): `up` starts
a **keeper** fence whose script is `( <up> ) </dev/null 3>&- 2>&1; echo READY rc=$?; exec
loop-harness serve-inner`. `serve-inner` reads requests on stdin and writes frames on fd 3 (up's
own processes have fd 3 closed, so they cannot write into the frames). Later `run` and `down`
requests run **inside the keeper**: same pid namespace, mounts and `/tmp`, each command in its own
process group with its own timeout. Without a keeper (no `up` yet, or after `down`) a command gets
a one-shot fence of its own. Network is the host's in both, so commands reach the sandbox over HTTP.

At the end the runner kills every fence of the session; killing the keeper (its namespace's init)
takes everything `up` started with it, `setsid` or not. Then, if `up` ever ran, `down` runs in a
fresh fence: its pidfile checks find nothing, and `--purge` removes the files. The alternative
(fences without `--unshare-pid`, host pids) would make `down` work everywhere but leak jarvisd
whenever `down` fails.

## The guard

`loop-harness` itself needs nothing from the guard, but what it asks the runner to run happens
outside the agent's sandbox and its deny rules (which match only the start of the command line).
So the guard checks `loop-harness run <cmd>` as it checks `bash -c <cmd>`: `adb` (which would
reach the user's emulator through the host's adb server), `ssh`, `sudo`, `git push`, `gh pr merge`
and the rest are refused there too. Pushing would fail anyway (the fence has no credentials).

## Not decided here

- `up` does not take the verify lock: the keeper lives for the agent's whole run, and holding the
  lock that long would stall other items' verify. GPU contention between an agent's harness and
  another item's verify is possible.
- A review agent can change files in its worktree through `loop-harness run` (the fence makes the
  worktree writable); this item does not change how a review's result is checked.
