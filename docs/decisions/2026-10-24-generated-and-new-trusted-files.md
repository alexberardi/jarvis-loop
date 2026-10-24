# Generated trusted files, and new files under a trusted dir

2026-10-24 · item #593. Since #537 the guard refuses every write under jarvis-server's
`deny_write` `tools/pipeline` (2026-10-23-guard-writes-every-channel.md), and verify refuses any
change there without the user's OK (2026-10-16-trusted-files.md). So a build that adds a chat tool
could neither write `tools/pipeline/toolsets/desktop.json`, the output of `go run ./tools/pipeline
toolset desktop` that CI's `TestDesktopToolsetIsCurrent` checks, nor add a pipeline scenario
(#573, #528). Enforced by `internal/guard` and `internal/runner` unit tests and
`scenarios/99-generated-files.json`.

## What was chosen

- **`generated`** (per repo: `paths`, globs as `path.Match`, and `run`, one bash line that rewrites
  them all in place). The build's guard lets the agent write those paths on every channel, even
  under a `deny_write` prefix. Verify trusts a change whose trusted paths are *all* generated ones
  only when `run`, fenced in the worktree at the head, leaves the worktree exactly as the head has
  it (`git status --porcelain --untracked-files=all` empty): the agent's bytes are the generator's.
  A failing generator, other bytes, a new file, or any other trusted path changed (the generator
  itself) keeps the whole change the user's, as before. The worktree is reset to the head
  afterwards either way.
- **`new_ok`** (per repo: `deny_write` prefixes). The build's guard lets the agent add a file under
  one that the base branch does not have; the base's own files there stay refused, judged against
  the base tip (`git ls-tree`, written into `guard.json` as `base_files`), not the disk, so the
  agent can keep editing a file it added. Verify still refuses the change until the user's Trust OK,
  as for any trusted file.
- The built-in prefixes (`.git`, `.claude`, `.mcp.json`, `.loop`) are never carved out.

For jarvis-server, the runner config (the user's `runner.json`) gains:

```json
"generated": {"paths": ["tools/pipeline/toolsets/desktop.json"],
              "run": "go run ./tools/pipeline toolset desktop > tools/pipeline/toolsets/desktop.json"},
"new_ok": ["tools/pipeline/scenarios"]
```

## Left as it is

- **Land still holds.** `pipeline.DefaultHoldRules` marks `tools/pipeline/**` trusted-file, so a
  PR with a regenerated toolset still waits for the user's approval at land. Whether a
  generated-only diff verify reproduced should land without it is the user's call; this change
  only removes the build's refusal and verify's Trust stop.
- **A generator that names its own outputs** (`{name}` per file) was not built: one `run` line that
  rewrites every generated file is enough for the one toolset there is.
