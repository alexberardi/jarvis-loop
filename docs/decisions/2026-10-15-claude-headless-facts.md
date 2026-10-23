# Claude Code headless facts, checked before phase 4

2026-10-15 · phase 4 (runner). Checked on the dev box against the installed CLI (`claude --version`
→ **2.1.287**), its `--help` and `auto-mode --help`, and the docs pages headless, permission-modes,
permissions, hooks, sandboxing, settings-reference, cli-reference, errors and the Agent SDK
TypeScript reference (code.claude.com/docs/en/*.md, fetched 2026-10-14). No agent session was
started; only `--help`, `--version` and `auto-mode --help` were run.

The docs describe newer releases than 2.1.287 in places; every feature below is checked against
the version it needs. **What the spec got wrong or left open is in bold.**

## Flags (from `claude --help`, 2.1.287)

| Flag | Fact | Used |
| --- | --- | --- |
| `-p` | non-interactive; skips the workspace trust dialog; **settings files that fail validation are silently ignored** in this mode | yes |
| `--permission-mode` | choices `acceptEdits`, `auto`, `bypassPermissions`, `manual` (alias of `default`), `dontAsk`, `plan` | `auto` (default) or `dontAsk` (`--mode strict`) |
| `--permission-prompts none` | (≥ 2.1.259) nobody answers prompts: anything that would prompt is denied, Claude is told not to retry, `AskUserQuestion` is removed | yes |
| `--settings <file-or-json>` | loads an extra settings source; it ranks with managed settings for the sandbox locks | yes, a generated file per run |
| `--setting-sources user,project,local` | which settings files load; `--settings` is separate | `user` only (see below) |
| `--allowedTools` / `--disallowedTools` | exist; equivalent to allow/deny rules | not used: the rules go in the generated settings |
| `--add-dir` | extra working directory: file tools and sandboxed writes reach it | **not used**: it would differ per item (prompt caching); the scratch dir is `.loop/scratch` inside the worktree |
| `--exclude-dynamic-system-prompt-sections` | moves cwd, env info, auto-memory location and git status from the system prompt into the first user message; only with the default system prompt | yes (prompt caching) |
| `--resume <id>` (`-r`) | resumes a session by id | supported by the runner's argv builder, not used by build runs |
| `--output-format stream-json` | NDJSON; the docs always pair it with `--verbose` | `--output-format stream-json --verbose` |
| `--max-turns` | **not in `--help`**; documented in cli-reference ("print mode only, exits with an error") | not used: loopd's time cap bounds a run |
| `--strict-mcp-config` | only `--mcp-config` servers | yes, with no `--mcp-config`: no MCP servers |
| `--bare` | **skips hooks** (so the guard would never run) and **never reads OAuth or the keychain**: auth is strictly `ANTHROPIC_API_KEY` or an `apiKeyHelper` | **not used**: it would drop subscription auth and layer 3 |
| `--restricted` | (≥ 2.1.248) only managed settings and `--settings`, file tools confined to working dirs, removes Bash unless `--tools` names it | not used yet; candidate after the first real run (decision file `2026-10-15-runner-choices.md`) |
| `--permission-prompt-tool` | MCP tool answering prompts | not used |
| `--dangerously-skip-permissions` | bypass | never; settings also set `disableBypassPermissionsMode: "disable"` |

## Permission modes

- **`auto` works in `-p`** on a direct Anthropic connection from 2.1.281 (installed 2.1.287), "as
  Anthropic rolls it out", on all plans. **If auto mode is unavailable** (server-side off, a
  settings file with `disableAutoMode`, an unsupported model), **the session silently starts in
  Manual (`default`)**; in `-p` with `--permission-prompts none` that means everything that would
  prompt is denied. The runner therefore reads `permissionMode` from the stream's `system/init`
  event and kills the run if it is not the mode it asked for (`mode_mismatch`), rather than letting
  an agent flail in Manual.
- Without `--permission-mode`, a `-p` run starts in `default` when feature flags are fetched, else
  `auto` (≥ 2.1.285). The runner always passes the mode.
- Evaluation order in auto: allow/ask/deny rules first (**deny always blocks**, in every mode,
  including writes the classifier would allow); then reads and working-dir edits are auto-approved;
  everything else goes to the classifier. Broad allow rules (`Bash(*)`, interpreters,
  package-manager run) are dropped in auto. Repeated classifier blocks (3 in a row / 20 total) in a
  `-p` run without a prompt tool: the action is not run and the run continues.
- `dontAsk`: only reads in working dirs, the read-only command set, allow rules and PreToolUse
  `allow` run; everything else is denied. That is the `--mode strict` fallback.
- `autoMode` (classifier rules, `classifyAllShell`) is **user or managed scope only**: it cannot be
  set from `--settings`. The user's own `autoMode.environment` (in `~/.claude/settings.json`) is
  loaded because the runner keeps `--setting-sources user`.
- Protected paths (`.git`, `.claude`, `.gitconfig`, shell rc files, `.mcp.json`, …) are never
  auto-approved; in `dontAsk` they are denied, in `auto` they go to the classifier.

## Hooks

- PreToolUse input on stdin: `tool_name`, `tool_input` (Write/Edit/Read `file_path` is always
  absolute and `~`-expanded; Bash `command`; NotebookEdit `notebook_path`; Grep/Glob `path`),
  `cwd`, `permission_mode`, `scratchpad_dir`.
- Decision: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",
  "permissionDecisionReason":"…"}}`; the reason is shown to Claude. Exit 2 with stderr also blocks,
  and exit 2 cannot be overridden by JSON. **Returning `allow` would skip the classifier**, so the
  guard never returns `allow`; it prints nothing and exits 0 to defer.
- **A hook that crashes, cannot start or times out is a non-blocking error: the tool call goes
  ahead.** `onFailure: "block"` fixes that **only from 2.1.295; the installed 2.1.287 does not have
  it**, and an unknown key could invalidate the whole settings file (silently ignored under `-p`).
  So: the guard fails closed by itself (any internal error → exit 2), the settings use the shell
  form (`args` was not confirmed for 2.1.287), and the runner proves the settings were applied: the
  guard writes an "armed" marker on `SessionStart` and on every PreToolUse call, and the runner
  kills a run whose first tool result arrives without it (`guard_not_armed`).
- In `-p`, a repository's project hooks, `env` block and `.mcp.json` servers **run even though the
  folder was never trusted**. jarvis-server's `.claude/settings.json` has a PostToolUse hook that
  runs `scripts/hooks/go-edit.sh` from the worktree, a file the agent can edit, and hooks run
  outside the sandbox. Hence `--setting-sources user` and `--strict-mcp-config`.

## Sandbox (Linux, bubblewrap + socat; both installed, no AppArmor userns restriction)

- Keys confirmed: `sandbox.enabled`, `failIfUnavailable`, `autoAllowBashIfSandboxed`,
  `allowUnsandboxedCommands`, `excludedCommands`, `filesystem.{allowWrite,denyWrite,denyRead,
  allowRead}`, `network.{allowedDomains,deniedDomains,strictAllowlist,allowLocalBinding}`.
- Defaults: writes to cwd, `--add-dir` dirs and a per-user `$TMPDIR`; reads everywhere
  (**credential files included**, so the deny list matters); network only through a proxy that
  checks hostnames; environment inherited.
- `failIfUnavailable: true`: refuse to start rather than run unsandboxed (without it, a missing
  bwrap silently runs commands unsandboxed). `allowUnsandboxedCommands: false` removes the
  `dangerouslyDisableSandbox` retry, and from `--settings` it makes the sandbox admin-required, so a
  repository's own settings cannot loosen it.
- `strictAllowlist: true` (honoured from `--settings`): hosts outside the allowlist are refused
  instead of going to the classifier's per-command host lists.
- **`allowLocalBinding` has no effect on Linux.** Each sandboxed command gets its own loopback: it
  can serve and connect to what *it* started, but not to servers on the host or started by an
  earlier command. **So the spec's "localhost binding (sandboxed jarvisd)" cannot work inside the
  sandbox:** `scripts/sandbox up` daemonizes jarvisd with `setsid` in one Bash call and the
  scenarios reach it from later calls, and `--llm shared` reaches the installed jarvisd on the host
  (`:7704`) and reads `~/.config/jarvis-harness/installed.env`, which the spec denies. See the
  runner decision file for what was done about it (nothing is silently disabled).
- In a linked git worktree the sandbox allows writes to the common `.git` dir so commits work;
  `hooks/` and `config` there stay write-denied.
- Paths: `/abs`, `~/x`; on Linux write-list entries with `*` are skipped (no globs).
- Hooks, MCP servers, the file tools and WebFetch run **outside** the sandbox; the sandbox covers
  Bash (and Monitor) only.

## Stream-json shapes (for the runner and the usage-limit matcher)

- First event `{"type":"system","subtype":"init","permissionMode":…,"tools":[…],"model":…,
  "apiKeySource":…,"session_id":…}` (SessionStart hook events may precede it).
  **`apiKeySource` is `"none"` on subscription auth** — confirmed 2026-10-23 over all 3334 real
  runs the runner has saved (CLI 2.1.287, every stage, Opus and Sonnet), with no other value
  ever: `cd <work_root>/runs; for f in */stream.jsonl; do head -5 "$f" | grep -m1
  '"subtype":"init"'; done | grep -o '"apiKeySource":"[^"]*"' | sort | uniq -c`. The runner
  asserts it (`wantAPIKeySource`): any other reading ends the run as `auth_mismatch` and pauses
  claims (decision file `2026-10-23-subscription-auth-only.md`).
- `{"type":"assistant","message":{…content…},"error":"rate_limit"|"overloaded"|"billing_error"|…}`.
- `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"|"allowed_warning"|"rejected",
  "resetsAt":<unix s>,"errorCode":"credits_required"?}}`.
- `{"type":"system","subtype":"api_retry","error":"rate_limit",…}`: a retry, not an end.
- `{"type":"system","subtype":"permission_denied",…}` for each denial.
- Last line `{"type":"result","subtype":"success"|"error_max_turns"|"error_during_execution"|…,
  "is_error":…,"num_turns":…,"duration_ms":…,"total_cost_usd":…,"result":"…","errors":[…],
  "api_error_status":429?,"permission_denials":[…]}`.
- Usage-limit texts (errors page): `You've hit your session limit · resets 3:45pm`, `… weekly limit
  · resets Mon 12:00am`, `… Opus limit …`, `… Sonnet limit …`, `You've hit your monthly spend
  limit · …`; and the older `Claude AI usage limit reached|<unix>`. **Not** a usage limit: `API
  Error: Server is temporarily limiting requests (not your usage limit)`, and `overloaded` (529).
- SIGTERM: exit 143, the turn is left unfinished, running Bash process trees are terminated,
  `SessionEnd` hooks run.

## Environment variables used

**Not** used, and scrubbed from every environment the runner hands out (`scrubEnv`): every
`ANTHROPIC_*` variable, and `CLAUDE_CODE_USE_BEDROCK` / `CLAUDE_CODE_USE_VERTEX`. The agents
authenticate with the OAuth login only; see `2026-10-23-subscription-auth-only.md`.

`CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` (no auto memory for loop agents), `ENABLE_CLAUDEAI_MCP_SERVERS=false`
(no claude.ai connectors; `--strict-mcp-config` covers configured servers and the `mcp__*` deny
rule removes any MCP tool that still appears). Both are documented in env-vars; their effect is
visible only in a real run's `system/init` (`mcp_servers`, `tools`).

## Not confirmable without a real run

Whether the Bash tool's description embeds the sandbox config (the settings are now the same for
every item of a repo, so it does not matter for caching); whether auto mode is actually granted to this account in `-p` today (the init event will say; the
runner refuses to run otherwise), and whether `-p` emits the `SessionStart` hook before `init`
(the armed check tolerates either order). **Answered since:** the `apiKeySource` value for
subscription auth is `"none"` (above).
