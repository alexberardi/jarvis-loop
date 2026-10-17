# Runner choices (phase 4)

2026-10-15 · phase 4 (runner and the build stage). Where the spec left room or turned out wrong
against the installed Claude Code (see `2026-10-15-claude-headless-facts.md`). Each is enforced by
a test unless it says otherwise. **Not yet run with a real agent**: held for the user's approval
of the boundaries.

**Superseded in part** by `2026-10-15-runner-pushes.md` (after the first real run): the agent no
longer pushes or opens the PR (`git push` and `gh pr create|edit` are denied outright; its outcome
is `ready_for_pr`, not `pr_opened`), `kill -0` is allowed, and a re-run continues the item's branch
from the mirror. The push and result rules below are the phase 4 originals.

## The four layers as built

**1. No loopd credential for the agent.** The runner's token is in its 0600 config (refused
otherwise) and only in the runner's own HTTP client. The agent's environment drops `LOOP_TOKEN`,
`LOOP_URL` and `SLACK_*`. The agent writes only `.loop/result.json` and `.loop/evidence/*`; the
runner validates and reports. Nothing the agent says is reported as is:

- `head_sha` must equal `git rev-parse HEAD` **and** the remote branch's head after a fetch (it was
  pushed); `changed_paths` must equal `git diff --name-status -M origin/<base>...HEAD` as a set;
  the runner reports git's values. A diff containing `.loop/` is refused.
- `repo` must be the configured slug, `branch` the item's branch, `pr` > 0, `summary` non-empty,
  only the stage's outcomes (`pr_opened`, `question`), no unknown fields.
- Evidence: regular files only (a symlink to `~/.ssh/id_ed25519` would otherwise be uploaded), at
  most 20, at most 20 MB each; the evidence dir itself must not be a symlink; result.json likewise.
- Outside the dry run, the PR is checked with the runner's own `gh pr view`: open, head ref = the
  item's branch, head oid = HEAD.

**2. Permissions** (generated settings, `--permission-mode auto`, `--permission-prompts none`):
deny rules (`internal/runner/settings.go`, golden files in `internal/runner/testdata/`):
`gh pr merge|review`, `gh repo delete|edit|rename|archive`, `gh auth|secret|ruleset|workflow`,
`gh api` with `-X`/`--method` `DELETE|PUT|PATCH` in every spelling, `git push` with `--force*`,
`-f`, `+refspec`, `--mirror`, `--all`, `--delete`, `-d`, or naming `main`/`master` (also as
`:main`, `refs/heads/main`), `adb`, `sudo`, `pkexec`, `doas`, `systemctl`, `kill`, `pkill`,
`killall`, `ssh`, `scp`, `sftp`, `rm -rf|-fr` of `/…`, `~…`, `..…`, `$HOME…`, `curl`/`wget` to
each spelling of loopd's address, `WebFetch`, `WebSearch`, `mcp__*`, `Read` of every secret path,
`Edit` of every protected path; `disableBypassPermissionsMode: "disable"`. Strict mode (`"mode":
"strict"` → `dontAsk`) adds the allowlist: `go`, `gofmt`, `npm`, `npx`, `git`, `gh pr
create|view|checks|list|comment`, `scripts/check`, `scripts/sandbox`, `scripts/fetch-sherpa-libs.sh`,
`mkdir`, `ls`, `Edit(./**)`.

- **Extra over the spec:** WebFetch/WebSearch and MCP tools are denied (exfiltration paths outside
  the sandbox's network proxy); `gh auth` (prints the token), `gh secret`, `gh workflow` are denied.
  Easy to loosen if an item needs the web.
- **Absolute `rm -rf` is denied by rule even inside the worktree** (`Bash(rm -rf /*)`); the brief
  tells the agent to use relative paths. The guard does the precise check.
- **Mode check:** the runner reads `permissionMode` from the stream's `system/init`. If auto mode
  was not granted (the CLI silently falls back to Manual), the run is killed, the item goes to the
  user with the reason, and **the runner stops claiming** until restarted (`mode_mismatch`).

**3. The guard** (`cmd/loop-guard`, `internal/guard`), on `PreToolUse` with matcher `*` and on
`SessionStart`. It never returns `allow` (that would skip the classifier); a call it does not
refuse exits 0 silently. A refusal exits 2 with a deny decision and the reason, and is appended to
the run's `refusals.jsonl` (outside the worktree), which the runner posts as `guard_refused`
events (new runner-only `POST /items/{id}/events`, lease holder only, kinds `guard_refused` and
`low_cache_hit`; Slack shows the rule and tool only, never the command). Rules:

- Write/Edit/MultiEdit/NotebookEdit (and any unknown tool with a `file_path`): the path, made
  absolute against the hook's `cwd`, cleaned and symlink-resolved through its longest existing
  prefix, must be inside the worktree, the sandbox dir (`.sandbox/loop-<id>`) or Claude Code's own
  session scratchpad under the temp dir; never `.git`, `.claude`, `.mcp.json`, `.loop/brief.md`,
  a repo's `deny_write` paths, or a protected path.
- Read/Grep/Glob/LS: refused into a secret path (a Glob pattern's static prefix too).
- Bash (and anything with a `command`): refused if it names a protected path in any spelling
  (`/abs`, `~/`, `$HOME/`, `${HOME}/`, whole path component only), the user's emulator serial, or
  loopd's address; then parsed (quotes, escapes, `;`, `&&`, `||`, `|`, `&`, subshells, `$(…)`,
  backquotes, `<(…)`, redirections, comments, here-documents; `bash|sh -c` and `eval` recursively;
  wrappers `env`, `timeout`, `nice`, `nohup`, `xargs`, `exec`, `command`, … stripped; program by
  basename) and each simple command checked: the never-list above; `git push` only as `git push
  [-u|-q|-v|--no-verify] origin <branch>|HEAD:<branch>|HEAD:refs/heads/<branch>` for the item's
  branch (no other flag, remote or refspec); `git remote add|set-url|…`, `git config` writes and
  `git worktree` (other than `list`) refused; `gh` as in layer 2 plus `gh api` mentioning `merge`
  or a GraphQL `mutation`; `rm -r` only inside the worktree, refusing paths with unexpanded
  variables and relative paths after a `cd`. Unparseable → refused.
- **Fail closed:** missing or bad config, unreadable input → exit 2. Claude Code treats any other
  hook failure as "go ahead" and `onFailure: "block"` needs 2.1.295 (installed: 2.1.287), so the
  runner also proves the settings were applied: the guard writes `armed` on every invocation and a
  run whose first tool result arrives without it is killed (`guard_not_armed`).

**4. The OS sandbox:** `enabled`, `failIfUnavailable`, `allowUnsandboxedCommands: false` (no
`dangerouslyDisableSandbox` retries; from `--settings` this also makes the sandbox admin-required,
so a repo's settings cannot loosen it), `autoAllowBashIfSandboxed: false` (sandboxed commands still
go to the classifier), writes beyond the defaults (worktree, `$TMPDIR`) only to `~/.cache/go-build`,
`~/go/pkg/mod`, `~/.npm`, `~/.cache/jarvis-harness`; `denyWrite` every protected path; `denyRead`
`~/.ssh`, `~/.config/jarvis-harness`, `~/.config/jarvis-loop`, `~/.claude/.credentials.json`, the
runner config, loopd's tokens file and data dir; network `strictAllowlist` with `github.com`,
`api.github.com`, `codeload.github.com`, `objects.githubusercontent.com`,
`raw.githubusercontent.com`, `proxy.golang.org`, `sum.golang.org`, `registry.npmjs.org`;
`deniedDomains` loopd's addresses. `excludedCommands`: none by default.

**What the sandbox blocks that the harness needs, and what was done** (spec: record, do not
silently disable). On Linux each sandboxed command has a private loopback and `allowLocalBinding`
does nothing, so jarvis-server's harness cannot run inside it: `scripts/sandbox up` starts jarvisd
with `setsid` in one Bash call and the scenarios reach it from later calls; `--llm shared` talks to
the installed jarvisd on the host's `:7704` and reads `~/.config/jarvis-harness/installed.env`,
which is read-denied. Nothing was loosened. The options, for the user to pick before jarvis-server
items run:

1. `excluded_commands` per repo (e.g. `scripts/sandbox *`, `go run ./tools/pipeline *`) with the
   harness entry points in `deny_write` (the guard refuses edits to them). Those commands then run
   **outside the sandbox** with the user's rights, still behind the deny rules, classifier and
   guard, and they build and run the agent's code. The config supports this; it is off.
2. Run up, scenarios and down in one Bash call (works in the sandbox for `--llm none`; the shared
   model would still need the host).
3. A runner-run harness step after the agent, outside the agent's session.

The first real item is in this repo, which has no harness, so none is needed yet.

**Since 2026-10-16** (item #67): none of the three. A repo's `harness` config gives its build and
review agents `loop-harness`, which asks the runner to run harness commands in its verify fence
over a Unix socket (the only one `allowUnixSockets` opens). `excludedCommands` stays empty.
See `2026-10-16-harness-broker.md`.

## The runner

- **Claims only `build`**: new optional `stages` on `POST /claim` (validated work stages;
  `pipeline.ClaimRequest.Stages`), table-tested. Without it a build runner would take triage items.
- **Layout** under `work_root` (`~/.cache/jarvis-loop/work`): `mirrors/<repo>.git` (a bare repo
  the runner owns: `init --bare`, origin = the configured https remote, objects seeded read-only
  from the user's clone with `git fetch <local>`, then fetched from origin; `commit.gpgsign` and
  `tag.gpgsign` false, `core.hooksPath=/dev/null`, `.loop/` in `info/exclude`),
  `<id>-build` (the worktree, `-B loop/<id>-<slug>` from the remote branch if an earlier run pushed
  one, else `origin/<base>`), `.loop/scratch` inside it, `runs/<id>-build-<time>/` (guard config,
  refusals, `stream.jsonl`, `stderr.log`, the brief; kept as the run's log, never writable by the
  agent; a retried stage keeps the refused attempt's as `stream-1.jsonl`, `stderr-1.log`,
  `brief-1.md`, see `2026-10-16-refused-results.md`), `settings/<repo>-build.json`. The user's checkout is never written: tests check its HEAD,
  status and branches after every scenario.
- **Unsigned commits:** the user's global git config signs with an SSH key under `~/.ssh`, which
  the sandbox denies, so the mirror turns signing off for loop branches.
- **Every runner git call** runs with `core.hooksPath=/dev/null`: the worktree is the agent's, and
  a planted hook must never run with the runner's rights. WIP commits are authored
  `jarvis-loop <jarvis-loop@localhost>`.
- **Prepare/cleanup commands** are per repo config (`prepare`, e.g. `scripts/fetch-sherpa-libs.sh
  linux-amd64 linux-arm64 darwin-arm64 windows-amd64`, run before the agent from the base branch's
  files; `cleanup`, e.g. `scripts/sandbox down --name {sandbox} --purge`, run after the files in
  `cleanup_restore` are checked out from the base branch, so an agent's edit to the harness never
  runs with the runner's rights). Known gap: a pidfile the agent wrote could still steer a
  harness's `down`; review before enabling cleanup for jarvis-server.
- **Process:** `claude` in its own process group (Setpgid); the brief on stdin; stream-json read
  line by line and logged to the run dir. Heartbeat every 60 s (refusals flushed first); `stop`,
  or a 4xx on the heartbeat (lease gone), → SIGTERM to the group, SIGKILL after 10 s; after any
  exit the group gets SIGKILL so nothing the agent started lingers. Only that group is signalled.
- **Time cap** = loopd's `max_run_minutes` minus 30 s, so the runner saves and reports before
  loopd's own overtime sweep: WIP pushed, outcome `failed` with the last output.
- **Stop / shutdown:** WIP commit (`git add -A`, `.loop/` excluded) pushed to the item's branch
  only, never forced; no report (loopd already ended the lease, or it will expire). Saving and
  reporting use a context that survives the runner's own shutdown.
- **Usage limit** (matcher fixtures in `testdata/limit_fixtures.jsonl`) → `usage_limit` with the
  error and reset (loopd sets active agents to 0). Not a limit: the "temporarily limiting requests
  (not your usage limit)" throttle, 529s, `api_retry`.
- **Reports carry** `run_seconds`, `exit_code`, `permission_mode`, `num_turns`, `duration_ms`,
  `total_cost_usd`, token usage, `session_id`, `guard_refusals`, `runner_agent`, `pr_verified`.
- **Repo lookup by key or slug:** loopd stores the PR's `owner/name` as the item's repo at
  `pr_opened`, so a bounced item is found by slug.
- **`--restricted`** (only managed + `--settings`, file tools confined to the working dirs) would
  tighten layer 2 further; not used until a real run shows what it does to the user's auto-mode
  environment and skills.
