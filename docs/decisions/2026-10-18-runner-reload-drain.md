# Reloading the runner's config, and draining before a binary update (#189)

**Why.** Every change to `runner.json` (a new repo, a slot, a verify step) needed a `loop-runner`
restart, and a restart ended every live agent run: a Claude build does not resume, so its work was
lost. On 2026-10-17 a restart to add three repos killed about 45 agent-minutes (#184, #169, #142,
#159). A binary update had the same cost.

## What changed

- **SIGHUP reloads `runner.json` in place** (`loop-runner reload` sends it). The file is read and
  checked exactly as at start (`LoadConfig`: mode 0600, no unknown field, the same normalising). A
  good file becomes the config of every **later claim**: repos (new and changed), slots, stages,
  verify, setup and harness steps, capabilities, the agent's binaries and mode. **Live runs keep the
  config they started with**: each run holds the config of its claim (`run.cfg`), so a reload in
  the middle of a build never changes its verify steps or its fence. The log says what was taken
  (`config reloaded`, the slots, stages, and the repos added or removed).
- **A bad file is refused, the old config kept**, with an error line (`config reload refused`):
  a parse error, a world-readable file, an invalid repo, or a change to a field only a restart can
  apply. Those are `name`, `loopd_url` and `token` (who the runner is to loopd), `work_root` (where
  every live run's worktree, mirror and logs are), and the fence: `bwrap`/`sandbox_exec` (probed
  once, at start) and `verify_mask`/`verify_writable` (which a live run's fenced commands read too).
- **Slots and stages re-size the lanes.** `Run` starts the agent lanes a reload adds (and the land
  lane when `land` joins the stages); a lane a reload takes away ends after its current claim, so
  fewer slots never stop a run.
- **Drain** (`loop-runner drain`, or SIGUSR1): the runner claims nothing new (not even land), tells
  loopd, and `Run` returns once the live runs end, the main-CI watches after a merge included
  (up to `land_main_wait`); the process then exits **0** for the service manager to start the new
  binary. Only SIGINT or SIGTERM stop runs, as before; a second signal of those during a drain
  still ends them.
- **The board shows a draining runner** ("Runner devbox is draining since …: it claims nothing new
  and restarts once its runs end"), and the orchestrator's snapshot carries its `state`. The runner
  says so with `POST /runners/self/state` (a runner-token route, so a Mac runner on
  `--runner-addr` can too); the runner's next claim, after the restart, clears it. The `runners`
  table gained `state` and `state_at` (migration 0019).
- **loopd reloads its repos on SIGHUP**: `--repos-file` is read again and added to `--repos` as at
  start, then given to the store (triage's choices, the CLI's `--repo`) and the Slack bot (`/loop
  add`, plans). An invalid file is refused with a log line and the repos stay.

## Choices

- **A pid file, not a control socket.** `loop-runner reload|drain` reads
  `<work_root>/loop-runner.pid` (written by a `Run`, not by `--once`) and signals that pid only if
  `ps` says it is a `loop-runner`, so a stale file never signals another program. A socket under
  the work root would be reachable by the agents, whose worktrees are there; signals are not (the
  agents' `kill` is refused). `systemctl --user kill -s HUP|USR1 loop-runner` works as well.
- **Exit 0 after a drain** needs a service that restarts on success: `Restart=always` in the
  systemd unit (`on-failure` would leave it stopped), `KeepAlive` true in the Mac's LaunchAgent
  (already so).
- **Not a hot binary swap**: drain then restart is enough, and needs no handing over of runs.

## Updating the runner

```sh
go build -o ~/.local/bin/loop-runner ./cmd/loop-runner   # the new binary
loop-runner drain                                         # live runs finish, then it exits 0
```

The service manager starts the new binary. A config change needs no restart: edit `runner.json`,
then `loop-runner reload` and read the log line.

## Not done

- loopd's other flags (tokens, Slack env) still need a restart; only the repos reload.
- A drain during a claim's request cancels it; if loopd had already leased the item, the lease
  expires and the item waits again, as after a restart.
- A draining runner still counts as seen for the unservable-items check: a drain lasts minutes.
