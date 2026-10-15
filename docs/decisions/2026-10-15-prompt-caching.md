# Prompt caching for loop agents

2026-10-15 · phase 4. The user's ask: an earlier agent setup used tokens much faster than manual
terminal use, probably through poor prompt caching. The runner now keeps every agent run's prefix
stable and records what each run actually cached.

## What the docs say (prompt-caching, cli-reference, env-vars; installed CLI 2.1.287)

- The API caches by exact prefix: system prompt (core instructions **and tool definitions**), then
  project context (CLAUDE.md, auto memory), then the conversation. A change early in the prefix
  recomputes everything after it.
- "The system prompt embeds your auto memory paths, and the conversation opens with an
  announcement of the working directory … Two sessions in different directories therefore build
  different prefixes." `--exclude-dynamic-system-prompt-sections` (in `--help`) moves cwd, env
  info, the memory location and git status into the first user message.
- Tool set: MCP servers and plugins change the tool definitions; a bare-name deny rule (e.g.
  `WebFetch`) removes a tool (with tool search active, definitions do not change). Permission
  modes, scoped deny rules and allow/ask rules do not touch the prefix.
- TTL: on a subscription within plan usage, the main conversation of a `-p` run gets the
  one-hour TTL by default; subagents five minutes.
- Model and effort each have their own cache.

## What the runner does

1. **The same argv, settings and tool set for every item of a stage and repo.** The argv has no
   per-item value (no `--add-dir`: the scratch dir is `.loop/scratch` in the worktree; the brief
   goes on stdin as the prompt, never `--append-system-prompt` or a per-item CLAUDE.md). The
   settings file is generated from the runner config and the repo only and written to
   `settings/<repo>-build.json`; the guard finds its per-run config through `$LOOP_GUARD_CONFIG`
   in the agent's environment (Claude Code passes its environment to hooks), so the hook command
   is just the guard's path. Whether hook commands or the sandbox config reach the prompt is not
   documented; with identical settings it no longer matters. No MCP: `--strict-mcp-config` with
   no `--mcp-config`, `ENABLE_CLAUDEAI_MCP_SERVERS=false`, and the `mcp__*` deny rule.
   `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`: a per-worktree memory dir would be new on every run.
   `--exclude-dynamic-system-prompt-sections` keeps cwd and git status out of the system prompt.
   Test: `TestAgentRunsShareTheirPrefix` prepares two items and compares argv (equal), settings
   bytes (equal), environment (only `LOOP_GUARD_CONFIG` differs), cwd (differs) and the brief
   (equal up to the item section); `TestSettingsHaveNoPerItemContent` and the golden files.
2. **Static first in the prompt.** The brief is the stage skill, then the result contract, then the
   item and the run's paths at the end.
3. **Measured.** From the stream's `result` event the runner records on every advance
   `usage_input`, `usage_output`, `usage_cache_creation`, `usage_cache_read`, `cache_hit_ratio` =
   read / (read + creation + input), `num_turns`, `duration_ms`, `total_cost_usd`, `run_seconds`,
   and the `session_id` (from `system/init`). A run with more than 5 turns and a ratio below 0.5
   also gets a `low_cache_hit` event (a warning; the digest will show these later).
4. **Context hygiene in the brief** ("Keep the context small"): quiet tests, long output to a file
   and read the tail, narrow reads, no pasted logs.
5. **Resumable sessions:** the argv builder takes `Resume` (`--resume <id>`) for the planning
   stages; build runs start fresh.

## Verified against the installed CLI vs. only documented

In `claude --help` (2.1.287): `--exclude-dynamic-system-prompt-sections`, `--strict-mcp-config`,
`--settings`, `--setting-sources`, `-r/--resume`, `--add-dir`, stream-json output. Documented only
(effect visible in a real run's `system/init` and `result.usage`): `ENABLE_CLAUDEAI_MCP_SERVERS`,
`CLAUDE_CODE_DISABLE_AUTO_MEMORY`, the one-hour TTL for `-p` on a subscription, that hooks inherit
Claude Code's environment, and the `result.usage` field names (Agent SDK reference). The dry run's
numbers are the fake's (79% read), not a measurement.
